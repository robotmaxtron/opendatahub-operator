/*
Copyright 2021 the original author or authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"

	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (s *ServiceBinding) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(s).
		WithDefaulter(ServiceBindingDefaulter).
		WithValidator(ServiceBindingValidator).
		Complete()
}

var ServiceBindingDefaulter webhook.CustomDefaulter = &ServiceBinding{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (s *ServiceBinding) Default(ctx context.Context, obj runtime.Object) error {
	if s.Spec.Name == "" {
		s.Spec.Name = s.Name
	}
	return nil
}

//+kubebuilder:webhook:path=/validate-servicebinding-io-v1-servicebinding,mutating=false,failurePolicy=fail,sideEffects=None,groups=servicebinding.io,resources=servicebindings,verbs=create;update,versions=v1,name=v1.servicebindings.servicebinding.io,admissionReviewVersions={v1,v1beta1}

var ServiceBindingValidator webhook.CustomValidator = &ServiceBinding{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (s *ServiceBinding) ValidateCreate(ctx context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	err = s.Default(ctx, obj)
	if err != nil {
		return nil, err
	}
	errs := s.validate()
	if len(errs) == 0 {
		return nil, nil
	}
	return nil, errs.ToAggregate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (s *ServiceBinding) ValidateUpdate(ctx context.Context, oldObj runtime.Object, newObj runtime.Object) (warnings admission.Warnings, err error) {
	err = s.Default(ctx, newObj)
	if err != nil {
		return nil, err
	}

	errs := field.ErrorList{}

	// check immutable fields
	var ro *ServiceBinding
	if o, ok := oldObj.(*ServiceBinding); ok {
		ro = o
	} else if o, ok := oldObj.(conversion.Convertible); ok {
		ro = &ServiceBinding{}
		if err = o.ConvertTo(ro); err != nil {
			return nil, err
		}
	} else {
		errs = append(errs,
			field.InternalError(nil, fmt.Errorf("old object must be of type v1.ServiceBinding")),
		)
	}
	if len(errs) == 0 {
		if s.Spec.Workload.APIVersion != ro.Spec.Workload.APIVersion {
			errs = append(errs,
				field.Forbidden(field.NewPath("spec", "workload", "apiVersion"), "Workload apiVersion is immutable. Delete and recreate the ServiceBinding to update."),
			)
		}
		if s.Spec.Workload.Kind != ro.Spec.Workload.Kind {
			errs = append(errs,
				field.Forbidden(field.NewPath("spec", "workload", "kind"), "Workload kind is immutable. Delete and recreate the ServiceBinding to update."),
			)
		}
	}

	// validate new object
	errs = append(errs, s.validate()...)

	return nil, errs.ToAggregate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (s *ServiceBinding) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (s *ServiceBinding) validate() field.ErrorList {
	errs := field.ErrorList{}

	errs = append(errs, s.Spec.validate(field.NewPath("spec"))...)

	return errs
}

func (s *ServiceBindingSpec) validate(fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	if s.Name == "" {
		errs = append(errs, field.Required(fldPath.Child("name"), ""))
	}
	errs = append(errs, s.Service.validate(fldPath.Child("service"))...)
	errs = append(errs, s.Workload.validate(fldPath.Child("workload"))...)
	for i := range s.Env {
		errs = append(errs, s.Env[i].validate(fldPath.Child("env").Index(i))...)
	}

	return errs
}

func (s *ServiceBindingServiceReference) validate(fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	if s.APIVersion == "" {
		errs = append(errs, field.Required(fldPath.Child("apiVersion"), ""))
	}
	if s.Kind == "" {
		errs = append(errs, field.Required(fldPath.Child("kind"), ""))
	}
	if s.Name == "" {
		errs = append(errs, field.Required(fldPath.Child("name"), ""))
	}

	return errs
}

func (s *ServiceBindingWorkloadReference) validate(fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	if s.APIVersion == "" {
		errs = append(errs, field.Required(fldPath.Child("apiVersion"), ""))
	}
	if s.Kind == "" {
		errs = append(errs, field.Required(fldPath.Child("kind"), ""))
	}
	if s.Name == "" && s.Selector == nil {
		errs = append(errs, field.Required(fldPath.Child("[name, selector]"), "expected exactly one, got neither"))
	}
	if s.Name != "" && s.Selector != nil {
		errs = append(errs, field.Required(fldPath.Child("[name, selector]"), "expected exactly one, got both"))
	}
	if s.Selector != nil {
		if _, err := metav1.LabelSelectorAsSelector(s.Selector); err != nil {
			errs = append(errs, field.Invalid(fldPath.Child("selector"), s.Selector, err.Error()))
		}
	}

	return errs
}

func (r *EnvMapping) validate(fldPath *field.Path) field.ErrorList {
	errs := field.ErrorList{}

	if r.Name == "" {
		errs = append(errs, field.Required(fldPath.Child("name"), ""))
	}
	if r.Key == "" {
		errs = append(errs, field.Required(fldPath.Child("key"), ""))
	}

	return errs
}

