package servicebindings

import (
	"context"
	"errors"
	"strings"
	"time"

	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"reconciler.io/runtime/duck"
	"reconciler.io/runtime/reconcilers"
	"reconciler.io/runtime/tracker"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	serviceApi "github.com/opendatahub-io/opendatahub-operator/v2/api/services/v1alpha1"
)

//+kubebuilder:rbac:groups=servicebinding.io,resources=servicebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=servicebinding.io,resources=servicebindings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=servicebinding.io,resources=servicebindings/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete

type ServiceBinding struct {
	Client client.Client
	ServiceBindingHooks
	SB         *serviceApi.ServiceBinding
	Reconciler func(reconcilersConfig, ServiceBindingHooks) *reconcilers.ResourceReconciler[*serviceApi.ServiceBinding]
	Config     reconcilersConfig
	clusterResolver
	ServiceBindingProjector
}

type reconcilersConfig struct {
	client.Client
	tracker.Tracker
	Recorder   record.EventRecorder
	APIReader  client.Reader
	SyncPeriod time.Duration
}

var syncPeriod = 10 * time.Hour

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceBinding) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	logf.FromContext(ctx).Info("Adding controller for ServiceBinding.")
	r.NewConfig(mgr, r.SB, syncPeriod)
	r.Config.Client = mgr.GetClient()
	r.Config.APIReader = mgr.GetAPIReader()
	r.Config.Recorder = mgr.GetEventRecorderFor("servicebinding-controller")
	r.Config.SyncPeriod = syncPeriod

	r.SB.InitializeConditions(ctx)
	if err := r.SB.SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	return nil
}

//+kubebuilder:rbac:groups=servicebinding.io,resources=servicebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=servicebinding.io,resources=servicebindings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=servicebinding.io,resources=servicebindings/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete

func (r *ServiceBinding) ServiceBindingReconciler(c reconcilers.Config) *reconcilers.ResourceReconciler[*serviceApi.ServiceBinding] {
	return &reconcilers.ResourceReconciler[*serviceApi.ServiceBinding]{
		Reconciler: &reconcilers.WithFinalizer[*serviceApi.ServiceBinding]{
			Finalizer: serviceApi.GroupVersion.Group + "/finalizer",
			Reconciler: reconcilers.Sequence[*serviceApi.ServiceBinding]{
				r.ResolveBindingSecret(),
				r.ResolveWorkloads(),
				r.ProjectBinding(),
				r.PatchWorkloads(),
			},
		},

		Config: c,
	}
}

func (r *ServiceBinding) ResolveBindingSecret() reconcilers.SubReconciler[*serviceApi.ServiceBinding] {
	return &reconcilers.SyncReconciler[*serviceApi.ServiceBinding]{
		Name: "ResolveBindingSecret",
		Sync: func(ctx context.Context, resource *serviceApi.ServiceBinding) error {
			secretName, err := r.GetResolver(TrackingClient(r.Config)).LookupBindingSecret(ctx, resource)
			if err != nil {
				if k8serr.IsNotFound(err) {
					// leave Unknown, the provisioned service may be created shortly
					resource.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionServiceAvailable, "ServiceNotFound")
					resource.GetConditionsManager().MarkUnknown(serviceApi.ServiceBindingConditionServiceAvailable)
					return nil
				}
				if k8serr.IsForbidden(err) {
					// set False, the operator needs to give access to the resource
					// see https://servicebinding.io/spec/core/1.0.0/#considerations-for-role-based-access-control-rbac
					resource.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionServiceAvailable, "ServiceForbidden")
					resource.GetConditionsManager().MarkFalse(serviceApi.ServiceBindingConditionServiceAvailable)
					return nil
				}
				// TODO handle other err cases
				return err
			}

			if secretName != "" {
				// success
				resource.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionServiceAvailable, "ResolvedBindingSecret")
				resource.GetConditionsManager().MarkTrue(serviceApi.ServiceBindingConditionServiceAvailable)
				previousSecretName := ""
				if resource.Status.Binding != nil {
					previousSecretName = resource.Status.Binding.Name
				}
				resource.Status.Binding = &serviceApi.ServiceBindingSecretReference{Name: secretName}
				if previousSecretName != secretName {
					// stop processing subreconcilers, we need to allow the secret to be updated on
					// the API Server so that webhook calls for workload that are targeted by the
					// binding are able to see this secret. The next turn of the reconciler for
					// this resource is automatically triggered by the change of status. We do not
					// want to requeue as that may cause the resource to be re-reconciled before
					// the informer cache is updated.
					return reconcilers.ErrHaltSubReconcilers
				}
			} else {
				// leave Unknown, not a success but also not an error
				resource.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionServiceAvailable, "ServiceMissingBinding")
				resource.GetConditionsManager().MarkUnknown(serviceApi.ServiceBindingConditionServiceAvailable)
				// TODO should we clear the existing binding?
				resource.Status.Binding = nil
			}

			return nil
		},
	}
}

func (r *ServiceBinding) ResolveWorkloads() reconcilers.SubReconciler[*serviceApi.ServiceBinding] {
	return &reconcilers.SyncReconciler[*serviceApi.ServiceBinding]{
		Name:                   "ResolveWorkloads",
		SyncDuringFinalization: true,
		SyncWithResult: func(ctx context.Context, resource *serviceApi.ServiceBinding) (reconcile.Result, error) {
			trackingRef := tracker.Reference{
				APIGroup:  schema.FromAPIVersionAndKind(resource.Spec.Workload.APIVersion, "").Group,
				Kind:      resource.Spec.Workload.Kind,
				Namespace: resource.Namespace,
			}
			if resource.Spec.Workload.Name != "" {
				trackingRef.Name = resource.Spec.Workload.Name
			}
			if resource.Spec.Workload.Selector != nil {
				selector, err := metav1.LabelSelectorAsSelector(resource.Spec.Workload.Selector)
				if err != nil {
					// should never get here
					return reconcile.Result{}, err
				}
				trackingRef.Selector = selector
			}
			if err := r.Config.TrackReference(trackingRef, resource); err != nil {
				return reconcile.Result{}, err
			}
			workloads, err := r.GetResolver(r.Config).LookupWorkloads(ctx, resource)
			if err != nil {
				if k8serr.IsForbidden(err) {
					// set False, the operator needs to give access to the resource
					// see https://servicebinding.io/spec/core/1.0.0/#considerations-for-role-based-access-control-rbac-1
					if resource.Spec.Workload.Name == "" {
						r.SB.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionWorkloadProjected, "WorkloadForbidden")
						r.SB.GetConditionsManager().MarkFalse(serviceApi.ServiceBindingConditionWorkloadProjected)
					} else {
						r.SB.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionWorkloadProjected, "WorkloadForbidden")
						r.SB.GetConditionsManager().MarkFalse(serviceApi.ServiceBindingConditionWorkloadProjected)
					}
					return reconcile.Result{}, nil
				}
				// TODO handle other err cases
				return reconcile.Result{}, err
			}
			if resource.Spec.Workload.Name != "" {
				found := false
				for _, workload := range workloads {
					if workload.(metav1.Object).GetName() == resource.Spec.Workload.Name {
						found = true
						break
					}
				}
				if !found {
					// leave Unknown, the workload may be created shortly
					r.SB.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionWorkloadProjected, "WorkloadNotFound")
					r.SB.GetConditionsManager().MarkUnknown(serviceApi.ServiceBindingConditionWorkloadProjected)
				}
			}

			StashWorkloads(ctx, workloads)

			return reconcile.Result{}, nil
		},
	}
}

//+kubebuilder:rbac:groups=servicebinding.io,resources=clusterworkloadresourcemappings,verbs=get;list;watch

func (r *ServiceBinding) ProjectBinding() reconcilers.SubReconciler[*serviceApi.ServiceBinding] {
	return &reconcilers.SyncReconciler[*serviceApi.ServiceBinding]{
		Name:                   "ProjectBinding",
		SyncDuringFinalization: true,
		Sync: func(ctx context.Context, resource *serviceApi.ServiceBinding) error {
			projector := r.GetProjector(r.GetResolver(r.Config))

			workloads := RetrieveWorkloads(ctx)
			projectedWorkloads := make([]runtime.Object, len(workloads))

			if f := r.ServiceBindingPreProjection; f != nil {
				if err := f(ctx, resource); err != nil {
					return err
				}
			}
			for i := range workloads {
				workload := workloads[i].DeepCopyObject()

				if f := r.WorkloadPreProjection; f != nil {
					if err := f(ctx, workload); err != nil {
						return err
					}
				}
				if !resource.DeletionTimestamp.IsZero() {
					if err := projector.Unproject(ctx, resource, workload); err != nil {
						return err
					}
				} else {
					if err := projector.Project(ctx, resource, workload); err != nil {
						return err
					}
				}
				if f := r.WorkloadPostProjection; f != nil {
					if err := f(ctx, workload); err != nil {
						return err
					}
				}

				projectedWorkloads[i] = workload
			}
			if f := r.ServiceBindingPostProjection; f != nil {
				if err := f(ctx, resource); err != nil {
					return err
				}
			}

			StashProjectedWorkloads(ctx, projectedWorkloads)

			return nil
		},

		Setup: func(ctx context.Context, mgr ctrl.Manager, bldr *builder.Builder) error {
			bldr.Watches(&serviceApi.ClusterWorkloadResourceMapping{}, handler.Funcs{})
			return nil
		},
	}
}

func (r *ServiceBinding) PatchWorkloads() reconcilers.SubReconciler[*serviceApi.ServiceBinding] {
	workloadManager := &reconcilers.OverrideSetup[*unstructured.Unstructured]{Name: "PatchWorkloads"}

	return &reconcilers.SyncReconciler[*serviceApi.ServiceBinding]{
		Name:                   "PatchWorkloads",
		SyncDuringFinalization: true,
		Sync: func(ctx context.Context, resource *serviceApi.ServiceBinding) error {
			workloads := RetrieveWorkloads(ctx)
			projectedWorkloads := RetrieveProjectedWorkloads(ctx)

			if len(workloads) != len(projectedWorkloads) {
				panic(errors.New("workloads and projectedWorkloads must have the same number of items"))
			}

			for i := range workloads {
				workload := workloads[i].(*unstructured.Unstructured)
				projectedWorkload := projectedWorkloads[i].(*unstructured.Unstructured)
				if workload.GetUID() != projectedWorkload.GetUID() || workload.GetResourceVersion() != projectedWorkload.GetResourceVersion() {
					panic(errors.New("workload and projectedWorkload must have the same uid and resourceVersion"))
				}

				if err := workloadManager.Validate(ctx); err != nil {
					if k8serr.IsNotFound(err) {
						// someone must have deleted the workload while we were operating on it
						continue
					}
					if k8serr.IsForbidden(err) {
						// set False, the operator needs to give access to the resource
						// see https://servicebinding.io/spec/core/1.0.0/#considerations-for-role-based-access-control-rbac-1
						r.SB.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionWorkloadProjected, "WorkloadForbidden")
						r.SB.GetConditionsManager().MarkFalse(serviceApi.ServiceBindingConditionWorkloadProjected)
						return nil
					}
					// TODO handle other err cases
					return err
				}
			}

			// update the WorkloadProjected condition to indicate success, but only if the condition has not already been set with another status
			cond := r.SB.GetCondition(serviceApi.ServiceBindingConditionWorkloadProjected)
			if cond.Status == metav1.ConditionUnknown && cond.Reason == "Initializing" {
				r.SB.GetConditionsManager().Mark(serviceApi.ServiceBindingConditionWorkloadProjected, "WorkloadProjected")
				r.SB.GetConditionsManager().MarkTrue(serviceApi.ServiceBindingConditionWorkloadProjected)
			}

			return nil
		},
	}
}

const WorkloadsStashKey reconcilers.StashKey = "servicebinding.io:workloads"

func StashWorkloads(ctx context.Context, workloads []runtime.Object) {
	reconcilers.StashValue(ctx, WorkloadsStashKey, workloads)
}

func RetrieveWorkloads(ctx context.Context) []runtime.Object {
	value := reconcilers.RetrieveValue(ctx, WorkloadsStashKey)
	if workloads, ok := value.([]runtime.Object); ok {
		return workloads
	}
	return nil
}

const ProjectedWorkloadsStashKey reconcilers.StashKey = "servicebinding.io:projected-workloads"

func StashProjectedWorkloads(ctx context.Context, workloads []runtime.Object) {
	reconcilers.StashValue(ctx, ProjectedWorkloadsStashKey, workloads)
}

func RetrieveProjectedWorkloads(ctx context.Context) []runtime.Object {
	value := reconcilers.RetrieveValue(ctx, ProjectedWorkloadsStashKey)
	if workloads, ok := value.([]runtime.Object); ok {
		return workloads
	}
	return nil
}

func (m *clusterResolver) GetResolver(c client.Client) Resolver {
	return m.NewResolver(c)
}

func (p *serviceBindingProjector) GetProjector(s MappingSource) ServiceBindingProjector {
	return p.NewProjector(s)
}

func (r *ServiceBinding) GetResolver(c client.Client) Resolver {
	return r.clusterResolver.GetResolver(c)
}

func (r *ServiceBinding) GetProjector(s MappingSource) ServiceBindingProjector {
	return r.ProjectorFactory(s)
}

func (c reconcilersConfig) IsEmpty() bool {
	return c == reconcilersConfig{}
}

// WithCluster extends the config to access a new cluster.
func (c reconcilersConfig) WithCluster(cluster cluster.Cluster) reconcilersConfig {
	return reconcilersConfig{
		Client:     duck.NewDuckAwareClientWrapper(cluster.GetClient()),
		APIReader:  duck.NewDuckAwareAPIReaderWrapper(cluster.GetAPIReader(), cluster.GetClient()),
		Recorder:   cluster.GetEventRecorderFor("controller"),
		Tracker:    c.Tracker,
		SyncPeriod: syncPeriod,
	}
}

// WithTracker extends the config with a new tracker.
func (c reconcilersConfig) WithTracker() reconcilersConfig {
	return reconcilersConfig{
		Client:    c.Client,
		APIReader: c.APIReader,
		Recorder:  c.Recorder,
		Tracker:   tracker.New(c.Scheme(), 2*syncPeriod),
	}
}

// WithDangerousDuckClientOperations returns a new Config with client Create and Update methods for
// duck typed objects enabled.
//
// This is dangerous because duck types typically represent a subset of the target resource and may
// cause data loss if the resource's server representation contains fields that do not exist on the
// duck typed object.
func (c reconcilersConfig) WithDangerousDuckClientOperations() reconcilersConfig {
	return reconcilersConfig{
		Client:    duck.NewDuckAwareClientWrapper(c.Client),
		APIReader: c.APIReader,
		Recorder:  c.Recorder,
		Tracker:   c.Tracker,

		SyncPeriod: syncPeriod,
	}
}

// TrackAndGet tracks the resources for changes and returns the current value.
//
//	The track is registered even when the resource does not exist so that its creation can be tracked.
//
// Equivalent to calling both `c.Tracker.TrackObject(...)` and `c.Client.Get(...)`.
func (c reconcilersConfig) TrackAndGet(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	// create a synthetic resource to track from known type and request
	req := reconcilers.RetrieveRequest(ctx)
	resource := reconcilers.RetrieveResourceType(ctx).DeepCopyObject().(client.Object)
	resource.SetNamespace(req.Namespace)
	resource.SetName(req.Name)
	ref := obj.DeepCopyObject().(client.Object)
	ref.SetNamespace(key.Namespace)
	ref.SetName(key.Name)
	if err := c.TrackObject(ref, resource); err != nil {
		return err
	}

	return c.Get(ctx, key, obj, opts...)
}

// TrackAndList tracks the resources for changes and returns the current value.
// Equivalent to calling both `c.Tracker.TrackReference(...)` and `c.Client.List(...)`.
func (c reconcilersConfig) TrackAndList(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	// create a synthetic resource to track from known type and request
	req := reconcilers.RetrieveRequest(ctx)
	resource := reconcilers.RetrieveResourceType(ctx).DeepCopyObject().(client.Object)
	resource.SetNamespace(req.Namespace)
	resource.SetName(req.Name)

	or, err := reference.GetReference(c.Scheme(), list)
	if err != nil {
		return err
	}
	gvk := schema.FromAPIVersionAndKind(or.APIVersion, or.Kind)
	listOpts := (&client.ListOptions{}).ApplyOptions(opts)
	if listOpts.LabelSelector == nil {
		listOpts.LabelSelector = labels.Everything()
	}
	ref := tracker.Reference{
		APIGroup:  gvk.Group,
		Kind:      strings.TrimSuffix(gvk.Kind, "List"),
		Namespace: listOpts.Namespace,
		Selector:  listOpts.LabelSelector,
	}
	if err = c.TrackReference(ref, resource); err != nil {
		return err
	}

	return c.List(ctx, list, opts...)
}

// NewConfig creates a Config for a specific API type. Typically passed into a reconciler.
func (r *ServiceBinding) NewConfig(mgr ctrl.Manager, apiType client.Object, syncPeriod time.Duration) {
	r.Config = reconcilersConfig{
		Client:     mgr.GetClient(),
		Tracker:    tracker.New(mgr.GetScheme(), syncPeriod),
		Recorder:   mgr.GetEventRecorderFor("controller"),
		APIReader:  mgr.GetAPIReader(),
		SyncPeriod: syncPeriod,
	}
}

func TrackingClient(config reconcilersConfig) client.Client {
	return &trackingClient{config}
}

type trackingClient struct {
	reconcilersConfig
}

func (c *trackingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.TrackAndGet(ctx, key, obj, opts...)
}

func (c *trackingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.TrackAndList(ctx, list, opts...)
}
