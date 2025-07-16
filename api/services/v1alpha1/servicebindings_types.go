package v1alpha1

import (
	"context"
	"github.com/opendatahub-io/opendatahub-operator/v2/api/common"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/controller/conditions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reconciler.io/runtime/apis"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// These are valid conditions of ServiceBinding.
const (
	// ServiceBindingConditionReady means the ServiceBinding has projected the ProvisionedService
	// secret and the Workload is ready to start. It does not indicate the condition
	// of either the Service or the Workload resources referenced.
	ServiceBindingConditionReady = apis.ConditionReady
	// ServiceBindingConditionServiceAvailable means the ServiceBinding's service
	// reference resolved to a ProvisionedService and found a secret. It does not
	// indicate the condition of the Service.
	ServiceBindingConditionServiceAvailable = "ServiceAvailable"
	// ServiceBindingConditionWorkloadProjected means the ServiceBinding has projected
	// the ProvisionedService secret and the Workload is ready to start. It does not
	// indicate the condition of the Workload resources referenced.
	//
	// Not a standardized condition.
	ServiceBindingConditionWorkloadProjected = "WorkloadProjected"
)

var servicebindingCondSet = apis.NewLivingConditionSetWithHappyReason(
	"ServiceBound",
	ServiceBindingConditionServiceAvailable,
	ServiceBindingConditionWorkloadProjected,
)

var _ conversion.Hub = (*ServiceBinding)(nil)

func (s *ServiceBinding) Hub() {}

// ServiceBindingWorkloadReference defines a subset of corev1.ObjectReference with extensions
type ServiceBindingWorkloadReference struct {
	// API version of the referent.
	APIVersion string `json:"apiVersion"`
	// Kind of the referent.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	Kind string `json:"kind"`
	// Name of the referent.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names
	Name string `json:"name,omitempty"`
	// Selector is a query that selects the workload or workloads to bind the service to
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Containers describe which containers in a Pod should be bound to
	Containers []string `json:"containers,omitempty"`
}

// ServiceBindingServiceReference defines a subset of corev1.ObjectReference
type ServiceBindingServiceReference struct {
	// API version of the referent.
	APIVersion string `json:"apiVersion"`
	// Kind of the referent.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	Kind string `json:"kind"`
	// Name of the referent.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names
	Name string `json:"name"`
}

// ServiceBindingSecretReference defines a mirror of corev1.LocalObjectReference
type ServiceBindingSecretReference struct {
	// Name of the referent secret.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names
	Name string `json:"name"`
}

// EnvMapping defines a mapping from the value of a Secret entry to an environment variable
type EnvMapping struct {
	// Name is the name of the environment variable
	Name string `json:"name"`
	// Key is the key in the Secret that will be exposed
	Key string `json:"key"`
}

// ServiceBindingSpec defines the desired state of ServiceBinding
type ServiceBindingSpec struct {
	// Name is the name of the service as projected into the workload container.  Defaults to .metadata.name.
	Name string `json:"name,omitempty"`
	// Type is the type of the service as projected into the workload container
	Type string `json:"type,omitempty"`
	// Provider is the provider of the service as projected into the workload container
	Provider string `json:"provider,omitempty"`
	// Workload is a reference to an object
	Workload ServiceBindingWorkloadReference `json:"workload"`
	// Service is a reference to an object that fulfills the ProvisionedService duck type
	Service ServiceBindingServiceReference `json:"service"`
	// Env is the collection of mappings from Secret entries to environment variables
	Env []EnvMapping `json:"env,omitempty"`
}

// ServiceBindingStatus defines the observed state of ServiceBinding
type ServiceBindingStatus struct {
	// ObservedGeneration is the 'Generation' of the ServiceBinding that
	// was last processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the conditions of this ServiceBinding
	Conditions []common.Condition `json:"conditions,omitempty"`

	// Binding exposes the projected secret for this ServiceBinding
	Binding *ServiceBindingSecretReference `json:"binding,omitempty"`
}

var _ common.ConditionsAccessor = (*ServiceBindingStatus)(nil)

// GetCondition fetches the condition of the specified type.
func (s *ServiceBinding) GetCondition(t string) *common.Condition {

	for _, cond := range s.Status.Conditions {
		if cond.Type == t {
			return &cond
		}
	}
	return nil
}

// GetConditions implements ConditionsAccessor
func (s *ServiceBindingStatus) GetConditions() []common.Condition {
	return s.Conditions
}

// SetConditions implements ConditionsAccessor
func (s *ServiceBindingStatus) SetConditions(c []common.Condition) {
	s.Conditions = c
}

// default kubebuilder markers for the new component
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.status.binding.name`
// +kubebuilder:printcolumn:name="Ready", type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ServiceBinding is the Schema for the ServiceBinding API
type ServiceBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceBindingSpec   `json:"spec,omitempty"`
	Status ServiceBindingStatus `json:"status,omitempty"`
}

func (s *ServiceBinding) GetConditions() []common.Condition {
	return s.Status.GetConditions()
}

func (s *ServiceBinding) SetConditions(c []common.Condition) {
	s.Status.SetConditions(c)
}

func (s *ServiceBinding) GetConditionSet() apis.ConditionSet {
	return servicebindingCondSet
}

func (s *ServiceBinding) GetConditionsAccessor() common.ConditionsAccessor {
	return &s.Status
}

func (s *ServiceBinding) GetStatus() *common.Status {
	return &common.Status{
		ObservedGeneration: s.GetGeneration(),
		Conditions:         s.Status.GetConditions(),
	}
}

func (s *ServiceBinding) GetReleaseStatus() *[]common.ComponentRelease {
	return &[]common.ComponentRelease{
		{
			Name:    "service-binding",
			Version: "1.0.1",
		},
	}
}

func (s *ServiceBinding) SetReleaseStatus(releases []common.ComponentRelease) {
	s.SetReleaseStatus(releases)
}

// +kubebuilder:object:root=true

// ServiceBindingList contains a list of ServiceBinding
type ServiceBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ServiceBinding `json:"items"`
}

// register the defined schemas
func init() {
	SchemeBuilder.Register(&ServiceBinding{}, &ServiceBindingList{})
}

// DSCServiceBinding contains all the configuration exposed in DSC instance for ExampleComponent component
type DSCServiceBinding struct {
	common.ManagementSpec `json:",inline"`
	ServiceBindingSpec    `json:",inline"`
}

// DSCServiceBindingStatus struct holds the status for the ServiceBinding component exposed in the DSC
type DSCServiceBindingStatus struct {
	common.ManagementSpec `json:",inline"`
	ServiceBindingStatus  `json:",inline"`
}

func (s *ServiceBinding) InitializeConditions(context.Context) {
	accessor := s.GetConditionsAccessor()
	// reset existing managed conditions
	conditions.SetStatusCondition(accessor, common.Condition{Type: ServiceBindingConditionReady})
}

func (s *ServiceBinding) GetConditionsManager() *conditions.Manager {
	return &conditions.Manager{}
}
