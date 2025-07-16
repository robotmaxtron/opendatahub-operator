package servicebindings

import (
	"context"
	"fmt"

	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	serviceApi "github.com/opendatahub-io/opendatahub-operator/v2/api/services/v1alpha1"
)

type Resolver interface {
	// LookupRESTMapping returns the RESTMapping for the workload type. The rest mapping contains a GroupVersionResource which can
	// be used to fetch the workload mapping.
	LookupRESTMapping(ctx context.Context, obj runtime.Object) (*meta.RESTMapping, error)

	// LookupWorkloadMapping the mapping template for the workload. Typically, a ClusterWorkloadResourceMapping is defined for the
	//  workload's fully qualified resource `{resource}.{group}`.
	LookupWorkloadMapping(ctx context.Context, gvr schema.GroupVersionResource) (*serviceApi.ClusterWorkloadResourceMappingSpec, error)

	// LookupBindingSecret returns the binding secret name exposed by the service following the Provisioned Service duck-type
	// (`.status.binding.name`). If a direction binding is used (where the referenced service is itself a Secret) the referenced Secret is
	// returned without a lookup.
	LookupBindingSecret(ctx context.Context, serviceBinding *serviceApi.ServiceBinding) (string, error)

	// LookupWorkloads returns the referenced objects. Often an unstructured Object is used to sidestep issues with schemes and registered
	// types. The selector is mutually exclusive with the reference name. The UID of the ServiceBinding is used to find resources that
	// may have been previously bound but no longer match the query.
	LookupWorkloads(ctx context.Context, serviceBinding *serviceApi.ServiceBinding) ([]runtime.Object, error)
}

// NewResolver creates a new resolver backed by a controller-runtime client.
func (m *clusterResolver) NewResolver(client client.Client) Resolver {
	return &clusterResolver{
		client: client,
	}
}

type clusterResolver struct {
	client client.Client
}

func (m *clusterResolver) LookupRESTMapping(ctx context.Context, obj runtime.Object) (*meta.RESTMapping, error) {
	gvk, err := apiutil.GVKForObject(obj, m.client.Scheme())
	if err != nil {
		return nil, err
	}
	rm, err := m.client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	return rm, nil
}

func (m *clusterResolver) LookupWorkloadMapping(ctx context.Context, gvr schema.GroupVersionResource) (*serviceApi.ClusterWorkloadResourceMappingSpec, error) {
	wrm := &serviceApi.ClusterWorkloadResourceMapping{}

	if err := m.client.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group)}, wrm); err != nil {
		if !k8serr.IsNotFound(err) {
			return nil, err
		}
		wrm.Spec = serviceApi.ClusterWorkloadResourceMappingSpec{
			Versions: []serviceApi.ClusterWorkloadResourceMappingTemplate{
				{
					Version: "*",
				},
			},
		}
	}

	for i := range wrm.Spec.Versions {
		wrm.Spec.Versions[i].Default()
	}

	return &wrm.Spec, nil
}

func (m *clusterResolver) LookupBindingSecret(ctx context.Context, serviceBinding *serviceApi.ServiceBinding) (string, error) {
	serviceRef := serviceBinding.Spec.Service
	if serviceRef.APIVersion == "v1" && serviceRef.Kind == "Secret" {
		// direct secret reference
		return serviceRef.Name, nil
	}
	service := &unstructured.Unstructured{}
	service.SetAPIVersion(serviceRef.APIVersion)
	service.SetKind(serviceRef.Kind)
	if err := m.client.Get(ctx, client.ObjectKey{Namespace: serviceBinding.Namespace, Name: serviceRef.Name}, service); err != nil {
		return "", err
	}
	secretName, exists, err := unstructured.NestedString(service.UnstructuredContent(), "status", "binding", "name")
	// treat missing values as empty
	_ = exists
	return secretName, err
}

const (
	mappingAnnotationPrefix = "projector.servicebinding.io/mapping-"
)

func (m *clusterResolver) LookupWorkloads(ctx context.Context, serviceBinding *serviceApi.ServiceBinding) ([]runtime.Object, error) {
	workloadRef := serviceBinding.Spec.Workload

	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(workloadRef.APIVersion)
	// TODO this is unsafe if the ListKind doesn't follow this convention
	list.SetKind(fmt.Sprintf("%sList", workloadRef.Kind))

	var ls labels.Selector
	if workloadRef.Selector != nil {
		var err error
		ls, err = metav1.LabelSelectorAsSelector(workloadRef.Selector)
		if err != nil {
			return nil, err
		}
	}

	if err := m.client.List(ctx, list, client.InNamespace(serviceBinding.Namespace)); err != nil {
		return nil, err
	}
	var workloads []runtime.Object
	for i := range list.Items {
		workload := &list.Items[i]
		if annotations := workload.GetAnnotations(); annotations != nil {
			if _, ok := annotations[fmt.Sprintf("%s%s", mappingAnnotationPrefix, serviceBinding.UID)]; ok {
				workloads = append(workloads, workload)
				continue
			}
		}
		if workloadRef.Name != "" {
			if workload.GetName() == workloadRef.Name {
				workloads = append(workloads, workload)
			}
			continue
		}
		if ls.Matches(labels.Set(workload.GetLabels())) {
			workloads = append(workloads, workload)
		}
	}

	return workloads, nil
}
