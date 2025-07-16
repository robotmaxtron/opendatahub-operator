package servicebindings

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/jsonpath"
	"k8s.io/utils/ptr"

	serviceApi "github.com/opendatahub-io/opendatahub-operator/v2/api/services/v1alpha1"
)

type ServiceBindingProjector interface {
	// Project the service into the workload as defined by the ServiceBinding.
	Project(ctx context.Context, binding *serviceApi.ServiceBinding, workload runtime.Object) error
	// Unproject the service from the workload as defined by the ServiceBinding.
	Unproject(ctx context.Context, binding *serviceApi.ServiceBinding, workload runtime.Object) error
	// IsProjected returns true when the binding has been projected into the workload
	IsProjected(ctx context.Context, binding *serviceApi.ServiceBinding, workload runtime.Object) bool
}

type MappingSource interface {
	// LookupRESTMapping returns the RESTMapping for the workload type. The rest mapping contains a GroupVersionResource which can
	// be used to fetch the workload mapping.
	LookupRESTMapping(ctx context.Context, obj runtime.Object) (*meta.RESTMapping, error)

	// LookupWorkloadMapping the mapping template for the workload. Typically, a ClusterWorkloadResourceMapping is defined for the
	//  workload's fully qualified resource `{resource}.{group}`.
	LookupWorkloadMapping(ctx context.Context, gvr schema.GroupVersionResource) (*serviceApi.ClusterWorkloadResourceMappingSpec, error)
}

func MappingVersion(version string, mappings *serviceApi.ClusterWorkloadResourceMappingSpec) *serviceApi.ClusterWorkloadResourceMappingTemplate {
	wildcardMapping := serviceApi.ClusterWorkloadResourceMappingTemplate{Version: "*"}
	var mapping *serviceApi.ClusterWorkloadResourceMappingTemplate
	for _, v := range mappings.Versions {
		switch v.Version {
		case version:
			mapping = &v
		case "*":
			wildcardMapping = v
		}
	}
	if mapping == nil {
		// use wildcard version by default
		mapping = &wildcardMapping
	}

	mapping = mapping.DeepCopy()
	mapping.Default()

	return mapping
}

var _ MappingSource = (*staticMapping)(nil)

type staticMapping struct {
	workloadMapping *serviceApi.ClusterWorkloadResourceMappingSpec
	restMapping     *meta.RESTMapping
}

// NewStaticMapping returns a single ClusterWorkloadResourceMappingSpec for each lookup. It is useful for
// testing.
func NewStaticMapping(wm *serviceApi.ClusterWorkloadResourceMappingSpec, rm *meta.RESTMapping) MappingSource {
	if len(wm.Versions) == 0 {
		wm.Versions = []serviceApi.ClusterWorkloadResourceMappingTemplate{
			{
				Version: "*",
			},
		}
	}
	for i := range wm.Versions {
		wm.Versions[i].Default()
	}

	return &staticMapping{
		workloadMapping: wm,
		restMapping:     rm,
	}
}

func (m *staticMapping) LookupRESTMapping(ctx context.Context, obj runtime.Object) (*meta.RESTMapping, error) {
	return m.restMapping, nil
}

func (m *staticMapping) LookupWorkloadMapping(ctx context.Context, gvr schema.GroupVersionResource) (*serviceApi.ClusterWorkloadResourceMappingSpec, error) {
	return m.workloadMapping, nil
}

const (
	ServiceBindingRootEnv    = "SERVICE_BINDING_ROOT"
	Group                    = "servicebinding.io"
	VolumePrefix             = "servicebinding-"
	SecretAnnotationPrefix   = Group + "/secret-"
	TypeAnnotationPrefix     = Group + "/type-"
	ProviderAnnotationPrefix = Group + "/provider-"
	MappingAnnotationPrefix  = Group + "/mapping-"
	VolumeDefaultMode        = int32(0644)
)

var _ ServiceBindingProjector = (*serviceBindingProjector)(nil)

type serviceBindingProjector struct {
	mappingSource MappingSource
}

// NewProjector creates a servicebinding projector configured for the mapping source. The binding projector is typically created
// once and applied to multiple workloads.
func (p *serviceBindingProjector) NewProjector(mappingSource MappingSource) ServiceBindingProjector {
	return &serviceBindingProjector{
		mappingSource: mappingSource,
	}
}

func (p *serviceBindingProjector) Project(ctx context.Context, binding *serviceApi.ServiceBinding, workload runtime.Object) error {
	ctx, resourceMapping, version, err := p.lookupClusterMapping(ctx, workload)
	if err != nil {
		return err
	}

	// rather than attempt to merge an existing binding, unproject it
	if err = p.Unproject(ctx, binding, workload); err != nil {
		return err
	}

	if !p.shouldProject(binding, workload) {
		return nil
	}

	versionMapping := MappingVersion(version, resourceMapping)
	mpt, err := NewMetaPodTemplate(ctx, workload, versionMapping)
	if err != nil {
		return err
	}
	p.project(binding, mpt)

	if p.secretName(binding) != "" {
		if err = p.stashLocalMapping(binding, mpt, resourceMapping); err != nil {
			return err
		}
	}
	if err = mpt.WriteToWorkload(ctx); err != nil {
		return err
	}

	return nil
}

func (p *serviceBindingProjector) Unproject(ctx context.Context, binding *serviceApi.ServiceBinding, workload runtime.Object) error {
	resourceMapping, err := p.retrieveLocalMapping(binding, workload)
	if err != nil {
		return err
	}
	ctx, m, version, err := p.lookupClusterMapping(ctx, workload)
	if err != nil {
		return err
	}
	if resourceMapping == nil {
		// fall back to using the remote mappings, this isn't ideal as the mapping may have changed after the binding was originally projected
		resourceMapping = m
	}
	versionMapping := MappingVersion(version, resourceMapping)
	mpt, err := NewMetaPodTemplate(ctx, workload, versionMapping)
	if err != nil {
		return err
	}
	p.unproject(binding, mpt)

	if err = p.stashLocalMapping(binding, mpt, nil); err != nil {
		return err
	}
	if err = mpt.WriteToWorkload(ctx); err != nil {
		return err
	}

	return nil
}

func (p *serviceBindingProjector) IsProjected(ctx context.Context, binding *serviceApi.ServiceBinding, workload runtime.Object) bool {
	annotations := workload.(metav1.Object).GetAnnotations()
	if len(annotations) == 0 {
		return false
	}
	_, ok := annotations[fmt.Sprintf("%s%s", MappingAnnotationPrefix, binding.UID)]
	return ok
}

type mappingValue struct {
	WorkloadMapping *serviceApi.ClusterWorkloadResourceMappingSpec
	RESTMapping     *meta.RESTMapping
}

// lookupClusterMapping resolves the mapping from the context or from the cluster. This
// avoids redundant calls to the mappingSource for the same workload call when Unproject
// is called from Project. When the lookup is from the cluster, the value is stashed into
// the context for future lookups in this turn.
func (p *serviceBindingProjector) lookupClusterMapping(ctx context.Context, workload runtime.Object) (context.Context, *serviceApi.ClusterWorkloadResourceMappingSpec, string, error) {
	raw := ctx.Value(mappingValue{})
	if value, ok := raw.(mappingValue); ok {
		return ctx, value.WorkloadMapping, value.RESTMapping.Resource.Version, nil
	}
	rm, err := p.mappingSource.LookupRESTMapping(ctx, workload)
	if err != nil {
		return ctx, nil, "", err
	}
	wm, err := p.mappingSource.LookupWorkloadMapping(ctx, rm.Resource)
	if err != nil {
		return ctx, nil, "", err
	}
	ctx = context.WithValue(ctx, mappingValue{}, mappingValue{
		WorkloadMapping: wm,
		RESTMapping:     rm,
	})
	return ctx, wm, rm.Resource.Version, nil
}

func (p *serviceBindingProjector) shouldProject(binding *serviceApi.ServiceBinding, workload runtime.Object) bool {
	if p.secretName(binding) == "" {
		// no secret to bind
		return false
	}

	if binding.Spec.Workload.Name != "" {
		return binding.Spec.Workload.Name == workload.(metav1.Object).GetName()
	}
	if binding.Spec.Workload.Selector != nil {
		ls, err := metav1.LabelSelectorAsSelector(binding.Spec.Workload.Selector)
		if err != nil {
			// should never get here
			return false
		}
		return ls.Matches(labels.Set(workload.(metav1.Object).GetLabels()))
	}

	return false
}

func (p *serviceBindingProjector) project(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate) {
	p.projectVolume(binding, mpt)
	for i := range mpt.Containers {
		p.projectContainer(binding, mpt, &mpt.Containers[i])
	}
}

func (p *serviceBindingProjector) unproject(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate) {
	p.unprojectVolume(binding, mpt)
	for i := range mpt.Containers {
		p.unprojectContainer(binding, mpt, &mpt.Containers[i])
	}

	// cleanup annotations
	delete(mpt.PodTemplateAnnotations, p.secretAnnotationName(binding))
	delete(mpt.PodTemplateAnnotations, p.typeAnnotationName(binding))
	delete(mpt.PodTemplateAnnotations, p.providerAnnotationName(binding))
}

func (p *serviceBindingProjector) projectVolume(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate) {
	volume := corev1.Volume{
		Name: p.volumeName(binding),
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To(VolumeDefaultMode),
				Sources: []corev1.VolumeProjection{
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: p.secretAnnotation(binding, mpt),
							},
						},
					},
				},
			},
		},
	}
	if binding.Spec.Type != "" {
		volume.Projected.Sources = append(volume.Projected.Sources,
			corev1.VolumeProjection{
				DownwardAPI: &corev1.DownwardAPIProjection{
					Items: []corev1.DownwardAPIVolumeFile{
						{
							Path: "type",
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: fmt.Sprintf("metadata.annotations['%s']", p.typeAnnotation(binding, mpt)),
							},
						},
					},
				},
			},
		)
	}
	if binding.Spec.Provider != "" {
		volume.Projected.Sources = append(volume.Projected.Sources,
			corev1.VolumeProjection{
				DownwardAPI: &corev1.DownwardAPIProjection{
					Items: []corev1.DownwardAPIVolumeFile{
						{
							Path: "provider",
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: fmt.Sprintf("metadata.annotations['%s']", p.providerAnnotation(binding, mpt)),
							},
						},
					},
				},
			},
		)
	}

	mpt.Volumes = append(mpt.Volumes, volume)

	// sort projected volumes
	sort.SliceStable(mpt.Volumes, func(i, j int) bool {
		ii := mpt.Volumes[i]
		jj := mpt.Volumes[j]
		ip := strings.HasPrefix(ii.Name, VolumePrefix)
		jp := strings.HasPrefix(jj.Name, VolumePrefix)
		if ip && jp {
			// sort projected items by name
			return ii.Name < jj.Name
		}
		if jp {
			// keep projected items after non-projected items
			return !ip
		}
		// preserve order of non-projected items
		return false
	})
}

func (p *serviceBindingProjector) unprojectVolume(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate) {
	volumes := []corev1.Volume{}
	projected := p.volumeName(binding)
	for _, v := range mpt.Volumes {
		if v.Name != projected {
			volumes = append(volumes, v)
		}
	}
	mpt.Volumes = volumes
}

func (p *serviceBindingProjector) projectContainer(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate, mc *metaContainer) {
	if !p.isContainerBindable(binding, mc) {
		return
	}
	p.projectVolumeMount(binding, mc)
	p.projectEnv(binding, mpt, mc)
}

func (p *serviceBindingProjector) unprojectContainer(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate, mc *metaContainer) {
	p.unprojectVolumeMount(binding, mc)
	p.unprojectEnv(binding, mpt, mc)
}

func (p *serviceBindingProjector) projectVolumeMount(binding *serviceApi.ServiceBinding, mc *metaContainer) {
	mc.VolumeMounts = append(mc.VolumeMounts, corev1.VolumeMount{
		Name:      p.volumeName(binding),
		ReadOnly:  true,
		MountPath: path.Join(p.serviceBindingRoot(mc), binding.Spec.Name),
	})

	// sort projected volume mounts
	sort.SliceStable(mc.VolumeMounts, func(i, j int) bool {
		ii := mc.VolumeMounts[i]
		jj := mc.VolumeMounts[j]
		ip := strings.HasPrefix(ii.Name, VolumePrefix)
		jp := strings.HasPrefix(jj.Name, VolumePrefix)
		if ip && jp {
			// sort projected items by name
			return ii.Name < jj.Name
		}
		if jp {
			// keep projected items after non-projected items
			return !ip
		}
		// preserve order of non-projected items
		return false
	})
}

func (p *serviceBindingProjector) unprojectVolumeMount(binding *serviceApi.ServiceBinding, mc *metaContainer) {
	mounts := []corev1.VolumeMount{}
	projected := p.volumeName(binding)
	for _, m := range mc.VolumeMounts {
		if m.Name != projected {
			mounts = append(mounts, m)
		}
	}
	mc.VolumeMounts = mounts
}

func (p *serviceBindingProjector) projectEnv(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate, mc *metaContainer) {
	for _, e := range binding.Spec.Env {
		if e.Key == "type" && binding.Spec.Type != "" {
			mc.Env = append(mc.Env, corev1.EnvVar{
				Name: e.Name,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: fmt.Sprintf("metadata.annotations['%s']", p.typeAnnotation(binding, mpt)),
					},
				},
			})
			continue
		}
		if e.Key == "provider" && binding.Spec.Provider != "" {
			mc.Env = append(mc.Env, corev1.EnvVar{
				Name: e.Name,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: fmt.Sprintf("metadata.annotations['%s']", p.providerAnnotation(binding, mpt)),
					},
				},
			})
			continue
		}
		mc.Env = append(mc.Env, corev1.EnvVar{
			Name: e.Name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: p.secretAnnotation(binding, mpt),
					},
					Key: e.Key,
				},
			},
		})
	}

	// sort projected env vars
	secrets := p.knownProjectedSecrets(mpt)
	sort.SliceStable(mc.Env, func(i, j int) bool {
		ii := mc.Env[i]
		jj := mc.Env[j]
		ip := p.isProjectedEnv(ii, secrets)
		jp := p.isProjectedEnv(jj, secrets)
		if ip && jp {
			// sort projected items by name
			return ii.Name < jj.Name
		}
		if jp {
			// keep projected items after non-projected items
			return !ip
		}
		// preserve order of non-projected items
		return false
	})
}

func (p *serviceBindingProjector) unprojectEnv(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate, mc *metaContainer) {
	env := []corev1.EnvVar{}
	secret := mpt.PodTemplateAnnotations[p.secretAnnotationName(binding)]
	typeFieldPath := fmt.Sprintf("metadata.annotations['%s']", p.typeAnnotationName(binding))
	providerFieldPath := fmt.Sprintf("metadata.annotations['%s']", p.providerAnnotationName(binding))
	for _, e := range mc.Env {
		// NB we do not remove the SERVICE_BINDING_ROOT env var since we don't know if someone else is depending on it
		remove := false
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == secret {
			// projected from secret
			remove = true
		}
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			if e.ValueFrom.FieldRef.FieldPath == typeFieldPath {
				// custom type env var
				remove = true
			}
			if e.ValueFrom.FieldRef.FieldPath == providerFieldPath {
				// custom provider env var
				remove = true
			}
		}
		if !remove {
			env = append(env, e)
		}
	}
	mc.Env = env
}

func (p *serviceBindingProjector) isContainerBindable(binding *serviceApi.ServiceBinding, mc *metaContainer) bool {
	if len(binding.Spec.Workload.Containers) == 0 || mc.Name == nil {
		return true
	}
	for _, name := range binding.Spec.Workload.Containers {
		if name == *mc.Name {
			return true
		}
	}
	return false
}

func (p *serviceBindingProjector) serviceBindingRoot(mc *metaContainer) string {
	for _, e := range mc.Env {
		if e.Name == ServiceBindingRootEnv {
			return e.Value
		}
	}
	// define default value
	serviceBindingRoot := corev1.EnvVar{
		Name:  ServiceBindingRootEnv,
		Value: "/bindings",
	}
	mc.Env = append(mc.Env, serviceBindingRoot)
	return serviceBindingRoot.Value
}

func (p *serviceBindingProjector) isProjectedEnv(e corev1.EnvVar, secrets sets.Set[string]) bool {
	if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && secrets.Has(e.ValueFrom.SecretKeyRef.Name) {
		// projected from secret
		return true
	}
	if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && strings.HasPrefix(e.ValueFrom.FieldRef.FieldPath, fmt.Sprintf("metadata.annotations['%s", Group)) {
		// projected custom type or annotation
		return true
	}
	return false
}

func (p *serviceBindingProjector) knownProjectedSecrets(mpt *metaPodTemplate) sets.Set[string] {
	secrets := sets.NewString()
	for k, v := range mpt.PodTemplateAnnotations {
		if strings.HasPrefix(k, SecretAnnotationPrefix) {
			secrets.Insert(v)
		}
	}
	return sets.Set[string](secrets)
}

func (p *serviceBindingProjector) secretName(binding *serviceApi.ServiceBinding) string {
	if binding.Status.Binding == nil {
		return ""
	}
	return binding.Status.Binding.Name
}

func (p *serviceBindingProjector) secretAnnotation(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate) string {
	key := p.secretAnnotationName(binding)
	secret := p.secretName(binding)
	if secret == "" {
		return ""
	}
	mpt.PodTemplateAnnotations[key] = secret
	return secret
}

func (p *serviceBindingProjector) secretAnnotationName(binding *serviceApi.ServiceBinding) string {
	return fmt.Sprintf("%s%s", SecretAnnotationPrefix, binding.UID)
}

func (p *serviceBindingProjector) volumeName(binding *serviceApi.ServiceBinding) string {
	return fmt.Sprintf("%s%s", VolumePrefix, binding.UID)
}

func (p *serviceBindingProjector) typeAnnotation(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate) string {
	key := p.typeAnnotationName(binding)
	mpt.PodTemplateAnnotations[key] = binding.Spec.Type
	return key
}

func (p *serviceBindingProjector) typeAnnotationName(binding *serviceApi.ServiceBinding) string {
	return fmt.Sprintf("%s%s", TypeAnnotationPrefix, binding.UID)
}

func (p *serviceBindingProjector) providerAnnotation(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate) string {
	key := p.providerAnnotationName(binding)
	mpt.PodTemplateAnnotations[key] = binding.Spec.Provider
	return key
}

func (p *serviceBindingProjector) providerAnnotationName(binding *serviceApi.ServiceBinding) string {
	return fmt.Sprintf("%s%s", ProviderAnnotationPrefix, binding.UID)
}

func (p *serviceBindingProjector) retrieveLocalMapping(binding *serviceApi.ServiceBinding, workload runtime.Object) (*serviceApi.ClusterWorkloadResourceMappingSpec, error) {
	annoations := workload.(metav1.Object).GetAnnotations()
	if annoations == nil {
		return nil, nil
	}
	data, ok := annoations[p.mappingAnnotationName(binding)]
	if !ok {
		return nil, nil
	}
	var mapping serviceApi.ClusterWorkloadResourceMappingSpec
	if err := json.Unmarshal([]byte(data), &mapping); err != nil {
		return nil, err
	}
	return &mapping, nil
}

func (p *serviceBindingProjector) stashLocalMapping(binding *serviceApi.ServiceBinding, mpt *metaPodTemplate, mapping *serviceApi.ClusterWorkloadResourceMappingSpec) error {
	if mapping == nil {
		delete(mpt.WorkloadAnnotations, p.mappingAnnotationName(binding))
		return nil
	}
	data, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	mpt.WorkloadAnnotations[p.mappingAnnotationName(binding)] = string(data)
	return nil
}

func (p *serviceBindingProjector) mappingAnnotationName(binding *serviceApi.ServiceBinding) string {
	return fmt.Sprintf("%s%s", MappingAnnotationPrefix, binding.UID)
}

type metaPodTemplate struct {
	workload runtime.Object
	mapping  *serviceApi.ClusterWorkloadResourceMappingTemplate

	WorkloadAnnotations    map[string]string
	PodTemplateAnnotations map[string]string
	Containers             []metaContainer
	Volumes                []corev1.Volume
}

// metaContainer contains the aspects of a Container that are appropriate for service binding.
type metaContainer struct {
	Name         *string
	Env          []corev1.EnvVar
	VolumeMounts []corev1.VolumeMount
}

// NewMetaPodTemplate coerces the workload object into a MetaPodTemplate following the mapping definition. The
// resulting MetaPodTemplate may have one or more service bindings applied to it at a time, but should not be reused.
// The workload must be JSON marshalable.
func NewMetaPodTemplate(ctx context.Context, workload runtime.Object, mapping *serviceApi.ClusterWorkloadResourceMappingTemplate) (*metaPodTemplate, error) {
	mpt := &metaPodTemplate{
		workload: workload,
		mapping:  mapping,

		WorkloadAnnotations:    map[string]string{},
		PodTemplateAnnotations: map[string]string{},
		Containers:             []metaContainer{},
		Volumes:                []corev1.Volume{},
	}

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(workload)
	if err != nil {
		return nil, err
	}
	uv := reflect.ValueOf(u)

	if err = mpt.getAt(".metadata.annotations", uv, &mpt.WorkloadAnnotations); err != nil {
		return nil, err
	}
	if err = mpt.getAt(mpt.mapping.Annotations, uv, &mpt.PodTemplateAnnotations); err != nil {
		return nil, err
	}
	for i := range mpt.mapping.Containers {
		cp := jsonpath.New("")
		if err = cp.Parse(fmt.Sprintf("{%s}", mpt.mapping.Containers[i].Path)); err != nil {
			return nil, err
		}
		cr, err := cp.FindResults(u)
		if err != nil {
			// errors are expected if a path is not found
			continue
		}
		for _, cv := range cr[0] {
			mc := metaContainer{
				Name:         nil,
				Env:          []corev1.EnvVar{},
				VolumeMounts: []corev1.VolumeMount{},
			}

			if mpt.mapping.Containers[i].Name != "" {
				// name is optional
				mc.Name = ptr.To("")
				if err = mpt.getAt(mpt.mapping.Containers[i].Name, cv, mc.Name); err != nil {
					return nil, err
				}
			}
			if err = mpt.getAt(mpt.mapping.Containers[i].Env, cv, &mc.Env); err != nil {
				return nil, err
			}
			if err = mpt.getAt(mpt.mapping.Containers[i].VolumeMounts, cv, &mc.VolumeMounts); err != nil {
				return nil, err
			}

			mpt.Containers = append(mpt.Containers, mc)
		}
	}
	if err = mpt.getAt(mpt.mapping.Volumes, uv, &mpt.Volumes); err != nil {
		return nil, err
	}

	return mpt, nil
}

// WriteToWorkload applies mutation defined on the MetaPodTemplate since it was created to the workload resource the
// MetaPodTemplate was created from. This method should generally be called once per instance.
func (mpt *metaPodTemplate) WriteToWorkload(ctx context.Context) error {
	// convert structured workload to unstructured
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(mpt.workload)
	if err != nil {
		return err
	}
	uv := reflect.ValueOf(u)

	if err := mpt.setAt(".metadata.annotations", &mpt.WorkloadAnnotations, uv); err != nil {
		return err
	}
	if err := mpt.setAt(mpt.mapping.Annotations, &mpt.PodTemplateAnnotations, uv); err != nil {
		return err
	}
	ci := 0
	for i := range mpt.mapping.Containers {
		cp := jsonpath.New("")
		if err := cp.Parse(fmt.Sprintf("{%s}", mpt.mapping.Containers[i].Path)); err != nil {
			return err
		}
		cr, err := cp.FindResults(u)
		if err != nil {
			// errors are expected if a path is not found
			continue
		}
		for _, cv := range cr[0] {
			if mpt.mapping.Containers[i].Name != "" && mpt.Containers[ci].Name != nil {
				if err := mpt.setAt(mpt.mapping.Containers[i].Name, mpt.Containers[ci].Name, cv); err != nil {
					return err
				}
			}
			if err := mpt.setAt(mpt.mapping.Containers[i].Env, &mpt.Containers[ci].Env, cv); err != nil {
				return err
			}
			if err := mpt.setAt(mpt.mapping.Containers[i].VolumeMounts, &mpt.Containers[ci].VolumeMounts, cv); err != nil {
				return err
			}

			ci++
		}
	}
	if err := mpt.setAt(mpt.mapping.Volumes, &mpt.Volumes, uv); err != nil {
		return err
	}

	// mutate workload with update content from unstructured
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u, mpt.workload)
}

func (mpt *metaPodTemplate) getAt(ptr string, source reflect.Value, target interface{}) error {
	parent := reflect.ValueOf(nil)
	createIfNil := false
	keys, err := mpt.keys(ptr)
	if err != nil {
		return err
	}
	v, _, _, err := mpt.find(source, parent, keys, "", createIfNil)
	if err != nil {
		return err
	}
	if !v.IsValid() || v.IsNil() {
		return nil
	}
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

func (mpt *metaPodTemplate) setAt(ptr string, value interface{}, target reflect.Value) error {
	keys, err := mpt.keys(ptr)
	if err != nil {
		return err
	}
	parent := reflect.ValueOf(nil)
	createIfNil := true
	_, vp, lk, err := mpt.find(target, parent, keys, "", createIfNil)
	if err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var out interface{}
	switch reflect.ValueOf(value).Elem().Kind() {
	case reflect.Map:
		out = map[string]interface{}{}
	case reflect.Slice:
		out = []interface{}{}
	case reflect.String:
		out = ""
	default:
		return fmt.Errorf("unsupported kind %s", reflect.ValueOf(value).Kind())
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	vp.SetMapIndex(reflect.ValueOf(lk), reflect.ValueOf(out))
	return nil
}

func (mpt *metaPodTemplate) keys(ptr string) ([]string, error) {
	p, err := jsonpath.Parse("", fmt.Sprintf("{%s}", ptr))
	if err != nil {
		return nil, err
	}
	return mpt.fieldKeys(p.Root)
}

func (mpt *metaPodTemplate) fieldKeys(node jsonpath.Node) ([]string, error) {
	switch node.Type() {
	case jsonpath.NodeList:
		list := node.(*jsonpath.ListNode)
		paths := []string{}
		for i := range list.Nodes {
			nestedpaths, err := mpt.fieldKeys(list.Nodes[i])
			if err != nil {
				return nil, err
			}
			paths = append(paths, nestedpaths...)
		}
		return paths, nil
	case jsonpath.NodeField:
		field := node.(*jsonpath.FieldNode)
		return []string{field.Value}, nil
	default:
		return nil, fmt.Errorf("unsupported node type %q found", node.Type())
	}
}

func (mpt *metaPodTemplate) find(value, parent reflect.Value, keys []string, lastKey string, createIfNil bool) (reflect.Value, reflect.Value, string, error) {
	if !value.IsValid() || value.IsNil() {
		if !createIfNil {
			return reflect.ValueOf(nil), reflect.ValueOf(nil), "", nil
		}
		value = reflect.ValueOf(make(map[string]interface{}))
		parent.SetMapIndex(reflect.ValueOf(lastKey), value)
	}
	if len(keys) == 0 {
		return value, parent, lastKey, nil
	}
	switch value.Kind() {
	case reflect.Map:
		lastKey = keys[0]
		keys = keys[1:]
		parent = value
		value = value.MapIndex(reflect.ValueOf(lastKey))
		return mpt.find(value, parent, keys, lastKey, createIfNil)
	case reflect.Interface:
		parent = value
		value = value.Elem()
		return mpt.find(value, parent, keys, lastKey, createIfNil)
	default:
		return reflect.ValueOf(nil), parent, lastKey, fmt.Errorf("unhandled kind %q", value.Kind())
	}
}
