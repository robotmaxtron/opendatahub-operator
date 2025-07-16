package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/util/jsonpath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (r *ClusterWorkloadResourceMapping) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		WithDefaulter(ClusterWorkloadDefaulter).
		WithValidator(ClusterWorkloadValidator).
		Complete()
}

var ClusterWorkloadDefaulter webhook.CustomDefaulter = &ClusterWorkloadResourceMapping{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *ClusterWorkloadResourceMapping) Default(ctx context.Context, obj runtime.Object) error {
	for i := range r.Spec.Versions {
		r.Spec.Versions[i].Default()
	}
	return nil
}

// Default applies values that are appropriate for a PodSpecable resource
func (r *ClusterWorkloadResourceMappingTemplate) Default() {
	if r.Annotations == "" {
		r.Annotations = ".spec.template.metadata.annotations"
	}
	if len(r.Containers) == 0 {
		r.Containers = []ClusterWorkloadResourceMappingContainer{
			{
				Path: ".spec.template.spec.initContainers[*]",
				Name: ".name",
			},
			{
				Path: ".spec.template.spec.containers[*]",
				Name: ".name",
			},
		}
	}
	for i := range r.Containers {
		c := &r.Containers[i]
		if c.Env == "" {
			c.Env = ".env"
		}
		if c.VolumeMounts == "" {
			c.VolumeMounts = ".volumeMounts"
		}
	}
	if r.Volumes == "" {
		r.Volumes = ".spec.template.spec.volumes"
	}
}

//+kubebuilder:webhook:path=/validate-servicebinding-io-v1-clusterworkloadresourcemapping,mutating=false,failurePolicy=fail,sideEffects=None,groups=servicebinding.io,resources=clusterworkloadresourcemappings,verbs=create;update,versions=v1,name=v1.clusterworkloadresourcemappings.servicebinding.io,admissionReviewVersions={v1,v1beta1}

var ClusterWorkloadValidator webhook.CustomValidator = &ClusterWorkloadResourceMapping{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *ClusterWorkloadResourceMapping) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	r.Default(ctx, obj)
	return nil, r.validate().ToAggregate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *ClusterWorkloadResourceMapping) ValidateUpdate(ctx context.Context, oldObj runtime.Object, newObj runtime.Object) (warnings admission.Warnings, err error) {
	r.Default(ctx, newObj)
	// TODO(user): check for immutable fields, if any
	return nil, r.validate().ToAggregate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *ClusterWorkloadResourceMapping) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (r *ClusterWorkloadResourceMapping) validate() field.ErrorList {
	errs := field.ErrorList{}

	versions := map[string]int{}
	for i := range r.Spec.Versions {
		// check for duplicate versions
		if p, ok := versions[r.Spec.Versions[i].Version]; ok {
			errs = append(errs, field.Duplicate(field.NewPath("spec", "versions", fmt.Sprintf("[%d, %d]", p, i), "version"), r.Spec.Versions[i].Version))
		}
		versions[r.Spec.Versions[i].Version] = i
		errs = append(errs, r.Spec.Versions[i].validate(field.NewPath("spec", "versions").Index(i))...)
	}

	return errs
}

func (r *ClusterWorkloadResourceMappingTemplate) validate(fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	if r.Version == "" {
		errs = append(errs, field.Required(fldPath.Child("version"), ""))
	}
	errs = append(errs, validateRestrictedJsonPath(r.Annotations, fldPath.Child("annotations"))...)
	errs = append(errs, validateRestrictedJsonPath(r.Volumes, fldPath.Child("volumes"))...)
	for i := range r.Containers {
		errs = append(errs, r.Containers[i].validate(fldPath.Child("containers").Index(i))...)
	}

	return errs
}

func (r *ClusterWorkloadResourceMappingContainer) validate(fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	errs = append(errs, validateJsonPath(r.Path, fldPath.Child("path"))...)
	if r.Name != "" {
		// name is optional
		errs = append(errs, validateRestrictedJsonPath(r.Name, fldPath.Child("name"))...)
	}
	errs = append(errs, validateRestrictedJsonPath(r.Env, fldPath.Child("env"))...)
	errs = append(errs, validateRestrictedJsonPath(r.VolumeMounts, fldPath.Child("volumeMounts"))...)

	return errs
}

func validateJsonPath(expression string, fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	if p, err := jsonpath.Parse("", fmt.Sprintf("{%s}", expression)); err != nil {
		errs = append(errs, field.Invalid(fldPath, expression, err.Error()))
	} else {
		if len(p.Root.Nodes) != 1 {
			errs = append(errs, field.Invalid(fldPath, expression, "too many root nodes"))
		}
	}

	return errs
}

func validateRestrictedJsonPath(expression string, fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	if p, err := jsonpath.Parse("", fmt.Sprintf("{%s}", expression)); err != nil {
		errs = append(errs, field.Invalid(fldPath, expression, err.Error()))
	} else {
		if len(p.Root.Nodes) != 1 {
			errs = append(errs, field.Invalid(fldPath, expression, "too many root nodes"))
		}
		// only allow jsonpath.NodeField nodes
		nodes := p.Root.Nodes
		for i := 0; i < len(nodes); i++ {
			switch n := nodes[i].(type) {
			case *jsonpath.ListNode:
				nodes = append(nodes, n.Nodes...)
			case *jsonpath.FieldNode:
				continue
			default:
				errs = append(errs, field.Invalid(fldPath, expression, fmt.Sprintf("unsupported node: %s", n)))
			}
		}
	}

	return errs
}
