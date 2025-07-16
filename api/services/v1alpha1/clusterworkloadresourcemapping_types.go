package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// ClusterWorkloadResourceMappingTemplate defines the mapping for a specific version of an workload resource to a
// logical PodTemplateSpec-like structure.
type ClusterWorkloadResourceMappingTemplate struct {
	// Version is the version of the workload resource that this mapping is for.
	Version string `json:"version"`
	// Annotations is a Restricted JSONPath that references the annotation map within the workload resource. These
	// annotations must end up in the resulting Pod and are generally not the workload resource's annotations.
	// Defaults to `.spec.template.metadata.annotations`.
	Annotations string `json:"annotations,omitempty"`
	// Containers is the collection of mappings to container-like fragments of the workload resource. Defaults to
	// mappings appropriate for a PodSpecable resource.
	Containers []ClusterWorkloadResourceMappingContainer `json:"containers,omitempty"`
	// Volumes is a Restricted JSONPath that references the slice of volumes within the workload resource. Defaults to
	// `.spec.template.spec.volumes`.
	Volumes string `json:"volumes,omitempty"`
}

// ClusterWorkloadResourceMappingContainer defines the mapping for a specific fragment of a workload resource
// to a Container-like structure.
//
// Each mapping defines exactly one path that may match multiple container-like fragments within the workload
// resource. For each object matching the path, the name, env and volumeMounts expressions are resolved to find those
// structures.
type ClusterWorkloadResourceMappingContainer struct {
	// Path is the JSONPath within the workload resource that matches an existing fragment that is container-like.
	Path string `json:"path"`
	// Name is a Restricted JSONPath that references the name of the container with the container-like workload resource
	// fragment. If not defined, container name filtering is ignored.
	Name string `json:"name,omitempty"`
	// Env is a Restricted JSONPath that references the slice of environment variables for the container with the
	// container-like workload resource fragment. The referenced location is created if it does not exist. Defaults
	// to `.envs`.
	Env string `json:"env,omitempty"`
	// VolumeMounts is a Restricted JSONPath that references the slice of volume mounts for the container with the
	// container-like workload resource fragment. The referenced location is created if it does not exist. Defaults
	// to `.volumeMounts`.
	VolumeMounts string `json:"volumeMounts,omitempty"`
}

// ClusterWorkloadResourceMappingSpec defines the desired state of ClusterWorkloadResourceMapping
type ClusterWorkloadResourceMappingSpec struct {
	// Versions is the collection of versions for a given resource, with mappings.
	Versions []ClusterWorkloadResourceMappingTemplate `json:"versions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterWorkloadResourceMapping is the Schema for the clusterworkloadresourcemappings API
type ClusterWorkloadResourceMapping struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterWorkloadResourceMappingSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterWorkloadResourceMappingList contains a list of ClusterWorkloadResourceMapping
type ClusterWorkloadResourceMappingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterWorkloadResourceMapping `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterWorkloadResourceMapping{}, &ClusterWorkloadResourceMappingList{})
}

var _ conversion.Hub = (*ClusterWorkloadResourceMapping)(nil)

// Hub for conversion
func (r *ClusterWorkloadResourceMapping) Hub() {}