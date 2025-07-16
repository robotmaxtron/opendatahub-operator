package servicebindings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	v1 "k8s.io/api/authorization/v1"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/servicebinding/runtime/rbac"
	"gomodules.xyz/jsonpatch/v3"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"reconciler.io/runtime/reconcilers"
	rtime "reconciler.io/runtime/time"
	"reconciler.io/runtime/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	serviceApi "github.com/opendatahub-io/opendatahub-operator/v2/api/services/v1alpha1"
)

type ServiceBindingHooks struct {
	// ResolverFactory returns a resolver which is used to look up binding
	// related values.
	//
	// +optional
	ResolverFactory func(client.Client) Resolver

	// ProjectorFactory returns a projector which is used to bind/unbind the
	// service to/from the workload.
	//
	// +optional
	ProjectorFactory func(MappingSource) ServiceBindingProjector

	// ServiceBindingPreProjection can be used to alter the resolved
	// ServiceBinding before the projection.
	//
	// +optional
	ServiceBindingPreProjection func(ctx context.Context, binding *serviceApi.ServiceBinding) error

	// ServiceBindingPostProjection can be used to alter the projected
	// ServiceBinding before mutations are persisted.
	//
	// +optional
	ServiceBindingPostProjection func(ctx context.Context, binding *serviceApi.ServiceBinding) error

	// WorkloadPreProjection can be used to alter the resolved workload before
	// the projection.
	//
	// +optional
	WorkloadPreProjection func(ctx context.Context, workload runtime.Object) error

	// WorkloadPostProjection can be used to alter the projected workload
	// before mutations are persisted.
	//
	// +optional
	WorkloadPostProjection func(ctx context.Context, workload runtime.Object) error

	accessChecker rbac.AccessChecker
}

type accessChecker struct {
	client client.Client
	verb   string
	ttl    time.Duration
	cache  map[v1.ResourceAttributes]v1.SelfSubjectAccessReview
	m      sync.Mutex
}

// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// TODO: Bring in rbac.AccessChecker
// AdmissionProjectorReconciler reconciles a MutatingWebhookConfiguration object.
func (r *ServiceBindingHooks) AdmissionProjectorReconciler(c reconcilers.Config, name string, accessChecker rbac.AccessChecker) *reconcilers.AggregateReconciler[*admissionregistrationv1.MutatingWebhookConfiguration] {
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name: name,
		},
	}

	return &reconcilers.AggregateReconciler[*admissionregistrationv1.MutatingWebhookConfiguration]{
		Name:    "AdmissionProjector",
		Request: req,
		Reconciler: &reconcilers.CastResource[*admissionregistrationv1.MutatingWebhookConfiguration, client.Object]{
			Reconciler: reconcilers.Sequence[client.Object]{
				r.LoadServiceBindings(req),
				r.InterceptGVKs(),
				r.WebhookRules([]admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update}, []string{}, accessChecker),
			},
		},
		DesiredResource: func(ctx context.Context, resource *admissionregistrationv1.MutatingWebhookConfiguration) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
			if resource == nil || len(resource.Webhooks) != 1 {
				// the webhook config isn't in a form that we expect, ignore it
				return resource, nil
			}
			rules := RetrieveWebhookRules(ctx)
			resource.Webhooks[0].Rules = rules
			return resource, nil
		},

		Setup: func(ctx context.Context, mgr ctrl.Manager, bldr *builder.Builder) error {
			if err := mgr.GetFieldIndexer().IndexField(ctx, &serviceApi.ServiceBinding{}, WorkloadRefIndexKey, WorkloadRefIndexFunc); err != nil {
				return err
			}
			return nil
		},
		Config: c,
	}
}

func (r *ServiceBinding) AdmissionProjectorWebhook(c reconcilers.Config, hooks ServiceBindingHooks) *reconcilers.AdmissionWebhookAdapter[*unstructured.Unstructured] {
	return &reconcilers.AdmissionWebhookAdapter[*unstructured.Unstructured]{
		Name: "AdmissionProjectorWebhook",
		Reconciler: &reconcilers.SyncReconciler[*unstructured.Unstructured]{
			Sync: func(ctx context.Context, workload *unstructured.Unstructured) error {
				c := reconcilers.RetrieveConfigOrDie(ctx)

				// find matching service bindings
				serviceBindings := &serviceApi.ServiceBindingList{}
				gvk := schema.FromAPIVersionAndKind(workload.GetAPIVersion(), workload.GetKind())
				if err := c.List(ctx, serviceBindings, client.InNamespace(workload.GetNamespace()), client.MatchingFields{WorkloadRefIndexKey: workloadRefIndexValue(gvk.Group, gvk.Kind)}); err != nil {
					return err
				}

				sbProjector := r.GetProjector(r.GetResolver(c))

				// check that bindings are for this workload
				var activeServiceBindings []serviceApi.ServiceBinding
				for _, sb := range serviceBindings.Items {
					if !sb.DeletionTimestamp.IsZero() {
						continue
					}
					if sbProjector.IsProjected(ctx, &sb, workload) {
						activeServiceBindings = append(activeServiceBindings, sb)
						continue
					}
					ref := sb.Spec.Workload
					if ref.Name == workload.GetName() {
						activeServiceBindings = append(activeServiceBindings, sb)
						continue
					}
					if ref.Selector != nil {
						selector, err := metav1.LabelSelectorAsSelector(ref.Selector)
						if err != nil {
							continue
						}
						if selector.Matches(labels.Set(workload.GetLabels())) {
							activeServiceBindings = append(activeServiceBindings, sb)
							continue
						}
					}
				}

				// project active bindings into workload
				if f := hooks.WorkloadPreProjection; f != nil {
					if err := f(ctx, workload); err != nil {
						return err
					}
				}
				for i := range activeServiceBindings {
					sb := activeServiceBindings[i].DeepCopy()
					sb.Default(ctx, sb)
					if f := hooks.ServiceBindingPreProjection; f != nil {
						if err := f(ctx, sb); err != nil {
							return err
						}
					}
					if err := r.Project(ctx, sb, workload); err != nil {
						return err
					}
					if f := hooks.ServiceBindingPostProjection; f != nil {
						if err := f(ctx, sb); err != nil {
							return err
						}
					}
				}
				if f := hooks.WorkloadPostProjection; f != nil {
					if err := f(ctx, workload); err != nil {
						return err
					}
				}

				return nil
			},
		},
		Config: c,
	}
}

//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete

// TriggerReconciler reconciles a ValidatingWebhookConfiguration object.
func (r *ServiceBindingHooks) TriggerReconciler(c reconcilers.Config, name string, accessChecker rbac.AccessChecker) *reconcilers.AggregateReconciler[*admissionregistrationv1.ValidatingWebhookConfiguration] {
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name: name,
		},
	}

	return &reconcilers.AggregateReconciler[*admissionregistrationv1.ValidatingWebhookConfiguration]{
		Name:    "Trigger",
		Request: req,
		Reconciler: &reconcilers.CastResource[*admissionregistrationv1.ValidatingWebhookConfiguration, client.Object]{
			Reconciler: reconcilers.Sequence[client.Object]{
				r.LoadServiceBindings(req),
				r.TriggerGVKs(),
				r.InterceptGVKs(),
				r.WebhookRules([]admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete}, []string{"status"}, accessChecker),
			},
		},
		DesiredResource: func(ctx context.Context, resource *admissionregistrationv1.ValidatingWebhookConfiguration) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
			if resource == nil || len(resource.Webhooks) != 1 {
				// the webhook config isn't in a form that we expect, ignore it
				return resource, nil
			}
			rules := RetrieveWebhookRules(ctx)
			resource.Webhooks[0].Rules = rules
			return resource, nil
		},

		Config: c,
	}
}

func (r *ServiceBindingHooks) TriggerWebhook(c reconcilers.Config, serviceBindingController controller.Controller) *reconcilers.AdmissionWebhookAdapter[*unstructured.Unstructured] {
	return &reconcilers.AdmissionWebhookAdapter[*unstructured.Unstructured]{
		Name: "TriggerWebhook",
		Reconciler: &reconcilers.SyncReconciler[*unstructured.Unstructured]{
			Sync: func(ctx context.Context, trigger *unstructured.Unstructured) error {
				log := logr.FromContextOrDiscard(ctx)
				c := reconcilers.RetrieveConfigOrDie(ctx)
				req := reconcilers.RetrieveAdmissionRequest(ctx)

				// TODO find a better way to get at the queue, this is fragile and may break in any controller-runtime update
				queueValue := reflect.ValueOf(serviceBindingController).Elem().FieldByName("Queue")
				if queueValue.IsNil() {
					// queue is not populated yet
					return nil
				}
				queue := queueValue.Interface().(workqueue.TypedInterface[reconcile.Request])

				obs, err := c.Tracker.GetObservers(trigger)
				if err != nil {
					return err
				}
				for _, ob := range obs {
					rr := reconcile.Request{NamespacedName: ob}
					log.V(2).Info("enqueue tracked request", "request", rr, "for", trigger, "dryRun", req.DryRun)
					if req.DryRun != nil && *req.DryRun {
						// ignore dry run requests
						continue
					}
					queue.Add(rr)
				}

				return nil
			},
		},
		Config: c,
	}
}

func (r *ServiceBindingHooks) LoadServiceBindings(req reconcile.Request) reconcilers.SubReconciler[client.Object] {
	return &reconcilers.SyncReconciler[client.Object]{
		Name: "LoadServiceBindings",
		Sync: func(ctx context.Context, _ client.Object) error {
			c := reconcilers.RetrieveConfigOrDie(ctx)

			serviceBindings := &serviceApi.ServiceBindingList{}
			if err := c.List(ctx, serviceBindings); err != nil {
				return err
			}

			StashServiceBindings(ctx, serviceBindings.Items)

			return nil
		},
		Setup: func(ctx context.Context, mgr ctrl.Manager, bldr *builder.Builder) error {
			bldr.Watches(&serviceApi.ServiceBinding{}, handler.EnqueueRequestsFromMapFunc(
				func(ctx context.Context, o client.Object) []reconcile.Request {
					return []reconcile.Request{req}
				},
			))
			return nil
		},
	}
}

func (r *ServiceBindingHooks) InterceptGVKs() reconcilers.SubReconciler[client.Object] {
	return &reconcilers.SyncReconciler[client.Object]{
		Name: "InterceptGVKs",
		Sync: func(ctx context.Context, _ client.Object) error {
			serviceBindings := RetrieveServiceBindings(ctx)
			gvks := RetrieveObservedGKVs(ctx)

			for i := range serviceBindings {
				workload := serviceBindings[i].Spec.Workload
				gvk := schema.FromAPIVersionAndKind(workload.APIVersion, workload.Kind)
				gvks = append(gvks, gvk)
			}

			StashObservedGVKs(ctx, gvks)

			return nil
		},
	}
}

func (r *ServiceBindingHooks) TriggerGVKs() reconcilers.SubReconciler[client.Object] {
	return &reconcilers.SyncReconciler[client.Object]{
		Name: "TriggerGVKs",
		Sync: func(ctx context.Context, _ client.Object) error {
			serviceBindings := RetrieveServiceBindings(ctx)
			gvks := RetrieveObservedGKVs(ctx)

			for i := range serviceBindings {
				service := serviceBindings[i].Spec.Service
				gvk := schema.FromAPIVersionAndKind(service.APIVersion, service.Kind)
				if gvk.Kind == "Secret" && (gvk.Group == "" || gvk.Group == "core") {
					// ignore direct bindings
					continue
				}
				gvks = append(gvks, gvk)
			}

			StashObservedGVKs(ctx, gvks)

			return nil
		},
	}
}

func (r *ServiceBindingHooks) WebhookRules(operations []admissionregistrationv1.OperationType, subresources []string, accessChecker rbac.AccessChecker) reconcilers.SubReconciler[client.Object] {
	return &reconcilers.SyncReconciler[client.Object]{
		Name: "WebhookRules",
		Sync: func(ctx context.Context, _ client.Object) error {
			log := logr.FromContextOrDiscard(ctx)
			c := reconcilers.RetrieveConfigOrDie(ctx)

			// dedup gvks as gvrs
			gvks := RetrieveObservedGKVs(ctx)
			groupResources := map[string]map[string]interface{}{}
			for _, gvk := range gvks {
				rm, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
				if err != nil {
					return err
				}
				gvr := rm.Resource
				if _, ok := groupResources[gvr.Group]; !ok {
					groupResources[gvr.Group] = map[string]interface{}{}
				}
				groupResources[gvr.Group][gvr.Resource] = true
			}

			// normalize rules to a canonical form
			var rules []admissionregistrationv1.RuleWithOperations
			groups := sets.NewString()
			for group := range groupResources {
				groups.Insert(group)
			}
			for _, group := range groups.List() {
				resources := sets.NewString()
				for resource := range groupResources[group] {
					resources.Insert(resource)
				}

				// check that we have permission to interact with these resources. Admission webhooks bypass RBAC
				for _, resource := range resources.List() {
					if !accessChecker.CanI(ctx, group, resource) {
						log.Info("ignoring resource, access denied", "group", group, "resource", resource)
						resources.Delete(resource)
					}
				}

				if resources.Len() == 0 {
					continue
				}
				for _, resource := range resources.List() {
					for _, subresource := range subresources {
						resources.Insert(fmt.Sprintf("%s/%s", resource, subresource))
					}
				}

				rules = append(rules, admissionregistrationv1.RuleWithOperations{
					Operations: operations,
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{group},
						APIVersions: []string{"*"},
						Resources:   resources.List(),
					},
				})
			}

			StashWebhookRules(ctx, rules)

			return nil
		},
	}
}

const ServiceBindingsStashKey reconcilers.StashKey = "servicebinding.io:servicebindings"

func StashServiceBindings(ctx context.Context, serviceBindings []serviceApi.ServiceBinding) {
	reconcilers.StashValue(ctx, ServiceBindingsStashKey, serviceBindings)
}

func RetrieveServiceBindings(ctx context.Context) []serviceApi.ServiceBinding {
	value := reconcilers.RetrieveValue(ctx, ServiceBindingsStashKey)
	if serviceBindings, ok := value.([]serviceApi.ServiceBinding); ok {
		return serviceBindings
	}
	return nil
}

const ObservedGVKsStashKey reconcilers.StashKey = "servicebinding.io:observedgvks"

func StashObservedGVKs(ctx context.Context, gvks []schema.GroupVersionKind) {
	reconcilers.StashValue(ctx, ObservedGVKsStashKey, gvks)
}

func RetrieveObservedGKVs(ctx context.Context) []schema.GroupVersionKind {
	value := reconcilers.RetrieveValue(ctx, ObservedGVKsStashKey)
	if refs, ok := value.([]schema.GroupVersionKind); ok {
		return refs
	}
	return nil
}

const WebhookRulesStashKey reconcilers.StashKey = "servicebinding.io:webhookrules"

func StashWebhookRules(ctx context.Context, rules []admissionregistrationv1.RuleWithOperations) {
	reconcilers.StashValue(ctx, WebhookRulesStashKey, rules)
}

func RetrieveWebhookRules(ctx context.Context) []admissionregistrationv1.RuleWithOperations {
	value := reconcilers.RetrieveValue(ctx, WebhookRulesStashKey)
	if rules, ok := value.([]admissionregistrationv1.RuleWithOperations); ok {
		return rules
	}
	return nil
}

const WorkloadRefIndexKey = ".metadata.workloadRef"

func WorkloadRefIndexFunc(obj client.Object) []string {
	serviceBinding := obj.(*serviceApi.ServiceBinding)
	gvk := schema.FromAPIVersionAndKind(serviceBinding.Spec.Workload.APIVersion, serviceBinding.Spec.Workload.Kind)
	return []string{
		workloadRefIndexValue(gvk.Group, gvk.Kind),
	}
}

func workloadRefIndexValue(group, kind string) string {
	return schema.GroupKind{Group: group, Kind: kind}.String()
}

type AdmissionWebhookAdapter[Type client.Object] struct {
	// Name used to identify this reconciler.  Defaults to `{Type}AdmissionWebhookAdapter`.  Ideally
	// unique, but not required to be so.
	//
	// +optional
	Name string

	// Type of resource to reconcile. Required when the generic type is not a struct.
	//
	// +optional
	Type Type

	// Reconciler is called for each reconciler request with the resource being reconciled.
	// Typically, Reconciler is a Sequence of multiple SubReconcilers.
	Reconciler reconcilers.SubReconciler[Type]

	// BeforeHandle is called first thing for each admission request.  A modified context may be
	// returned.
	//
	// If BeforeHandle is not defined, there is no effect.
	//
	// +optional
	BeforeHandle func(ctx context.Context, req admission.Request, resp *admission.Response) context.Context

	// AfterHandle is called following all work for the admission request. The response is provided
	// and may be modified before returning.
	//
	// If AfterHandle is not defined, the response is returned directly.
	//
	// +optional
	AfterHandle func(ctx context.Context, req admission.Request, resp *admission.Response)

	Config reconcilers.Config

	lazyInit sync.Once
}

func (r *AdmissionWebhookAdapter[T]) init() {
	r.lazyInit.Do(func() {
		if IsNil(r.Type) {
			var nilT T
			r.Type = newEmpty(nilT).(T)
		}
		if r.Name == "" {
			r.Name = fmt.Sprintf("%sAdmissionWebhookAdapter", typeName(r.Type))
		}
		if r.BeforeHandle == nil {
			r.BeforeHandle = func(ctx context.Context, req admission.Request, resp *admission.Response) context.Context {
				return ctx
			}
		}
		if r.AfterHandle == nil {
			r.AfterHandle = func(ctx context.Context, req admission.Request, resp *admission.Response) {}
		}
	})
}

func (r *AdmissionWebhookAdapter[T]) Validate(ctx context.Context) error {
	r.init()

	// validate Reconciler value
	if r.Reconciler == nil {
		return fmt.Errorf("AdmissionWebhookAdapter %q must define Reconciler", r.Name)
	}
	if validation.IsRecursive(ctx) {
		if v, ok := r.Reconciler.(validation.Validator); ok {
			if err := v.Validate(ctx); err != nil {
				return fmt.Errorf("AdmissionWebhookAdapter %q must have a valid Reconciler: %w", r.Name, err)
			}
		}
	}

	return nil
}

// Deprecated: use BuildWithContext.
func (r *AdmissionWebhookAdapter[T]) Build() *admission.Webhook {
	webhook, err := r.BuildWithContext(context.TODO())
	if err != nil {
		panic(err)
	}
	return webhook
}

func (r *AdmissionWebhookAdapter[T]) BuildWithContext(ctx context.Context) (*admission.Webhook, error) {
	r.init()

	if err := r.Validate(r.withContext(ctx)); err != nil {
		return nil, err
	}

	return &admission.Webhook{
		Handler: r,
		WithContextFunc: func(ctx context.Context, req *http.Request) context.Context {
			ctx = r.withContext(ctx)

			log := crlog.FromContext(ctx).
				WithValues(
					"webhook", req.URL.Path,
				)
			ctx = logr.NewContext(ctx, log)

			ctx = StashHTTPRequest(ctx, req)

			return ctx
		},
	}, nil
}

func (r *AdmissionWebhookAdapter[T]) withContext(ctx context.Context) context.Context {
	log := crlog.FromContext(ctx).
		WithName("controller-runtime.webhook.webhooks").
		WithName(r.Name)
	ctx = logr.NewContext(ctx, log)

	ctx = reconcilers.WithStash(ctx)

	ctx = reconcilers.StashConfig(ctx, r.Config)
	ctx = reconcilers.StashOriginalConfig(ctx, r.Config)
	ctx = reconcilers.StashResourceType(ctx, r.Type)
	ctx = reconcilers.StashOriginalResourceType(ctx, r.Type)

	return ctx
}

// Handle implements admission.Handler.
func (r *AdmissionWebhookAdapter[T]) Handle(ctx context.Context, req admission.Request) admission.Response {
	r.init()

	log := logr.FromContextOrDiscard(ctx).
		WithValues(
			"UID", req.UID,
			"kind", req.Kind,
			"resource", req.Resource,
			"operation", req.Operation,
		)
	ctx = logr.NewContext(ctx, log)

	resp := &admission.Response{
		AdmissionResponse: admissionv1.AdmissionResponse{
			UID: req.UID,
			// allow by default, a reconciler can flip this off, or return an err
			Allowed: true,
		},
	}

	ctx = rtime.StashNow(ctx, time.Now())
	ctx = StashAdmissionRequest(ctx, req)
	ctx = StashAdmissionResponse(ctx, resp)

	if beforeCtx := r.BeforeHandle(ctx, req, resp); beforeCtx != nil {
		ctx = beforeCtx
	}

	// defined for compatibility since this is not a reconciler
	ctx = reconcilers.StashRequest(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: req.Namespace,
			Name:      req.Name,
		},
	})

	if err := r.reconcile(ctx, req, resp); err != nil {
		if !errors.Is(err, reconcilers.ErrQuiet) {
			log.Error(err, "reconcile error")
		}
		resp.Allowed = false
		if resp.Result == nil {
			resp.Result = &metav1.Status{
				Code:    500,
				Message: err.Error(),
			}
		}
	}

	r.AfterHandle(ctx, req, resp)
	return *resp
}

func (r *AdmissionWebhookAdapter[T]) reconcile(ctx context.Context, req admission.Request, resp *admission.Response) error {
	log := logr.FromContextOrDiscard(ctx)

	resource := r.Type.DeepCopyObject().(T)
	resourceBytes := req.Object.Raw
	if req.Operation == admissionv1.Delete {
		resourceBytes = req.OldObject.Raw
	}
	if err := json.Unmarshal(resourceBytes, resource); err != nil {
		return err
	}

	if defaulter, ok := client.Object(resource).(webhook.CustomDefaulter); ok {
		// resource.Default(ctx, resource)
		if err := defaulter.Default(ctx, resource); err != nil {
			return err
		}
	} else if defaulter, ok := client.Object(resource).(objectDefaulter); ok {
		// resource.Default()
		defaulter.Default()
	}

	originalResource := resource.DeepCopyObject()
	if _, err := r.Reconciler.Reconcile(ctx, resource); err != nil {
		return err
	}

	if resp.Patches == nil && resp.Patch == nil && resp.PatchType == nil && !equality.Semantic.DeepEqual(originalResource, resource) {
		// add patch to response

		mutationBytes, err := json.Marshal(resource)
		if err != nil {
			return err
		}

		// create patch using jsonpatch v3 since it preserves order, then convert back to v2 used by AdmissionResponse
		patch, err := jsonpatch.CreatePatch(resourceBytes, mutationBytes)
		if err != nil {
			return err
		}
		data, _ := json.Marshal(patch)
		_ = json.Unmarshal(data, &resp.Patches)
		log.Info("mutating resource", "patch", resp.Patches)
	}

	return nil
}

const (
	admissionRequestStashKey  reconcilers.StashKey = "reconciler.io/runtime:admission-request"
	admissionResponseStashKey reconcilers.StashKey = "reconciler.io/runtime:admission-response"
	httpRequestStashKey       reconcilers.StashKey = "reconciler.io/runtime:http-request"
)

func StashAdmissionRequest(ctx context.Context, req admission.Request) context.Context {
	return context.WithValue(ctx, admissionRequestStashKey, req)
}

func StashAdmissionResponse(ctx context.Context, resp *admission.Response) context.Context {
	return context.WithValue(ctx, admissionResponseStashKey, resp)
}

func StashHTTPRequest(ctx context.Context, req *http.Request) context.Context {
	return context.WithValue(ctx, httpRequestStashKey, req)
}

func typeName(i interface{}) string {
	if obj, ok := i.(client.Object); ok {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if kind != "" {
			return kind
		}
	}

	t := reflect.TypeOf(i)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

// IsNil returns true if the value is nil, false if the value is not nilable or not nil.
func IsNil(val interface{}) bool {
	if val == nil {
		return true
	}
	if !IsNilable(val) {
		return false
	}
	return reflect.ValueOf(val).IsNil()
}

func IsNilable(val interface{}) bool {
	v := reflect.ValueOf(val)
	switch v.Kind() {
	case reflect.Chan:
		return true
	case reflect.Func:
		return true
	case reflect.Interface:
		return true
	case reflect.Map:
		return true
	case reflect.Ptr:
		return true
	case reflect.Slice:
		return true
	default:
		return false
	}
}

// replaceWithEmpty overwrite the underlying value with it's empty value.
func replaceWithEmpty(x interface{}) {
	v := reflect.ValueOf(x).Elem()
	v.Set(reflect.Zero(v.Type()))
}

// newEmpty returns a new empty value of the same underlying type, preserving the existing value.
func newEmpty(x interface{}) interface{} {
	t := reflect.TypeOf(x).Elem()
	return reflect.New(t).Interface()
}

type objectDefaulter interface {
	Default()
}