package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	annotation "github.com/opendatahub-io/opendatahub-operator/v2/pkg/metadata/annotations"
	oauthv1 "github.com/openshift/api/oauth/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/wait"
	"math/big"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"time"

	"github.com/opendatahub-io/opendatahub-operator/v2/api/common"
	componentApi "github.com/opendatahub-io/opendatahub-operator/v2/api/components/v1alpha1"
	dsciv1 "github.com/opendatahub-io/opendatahub-operator/v2/api/dscinitialization/v1"
	"github.com/opendatahub-io/opendatahub-operator/v2/internal/controller/status"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/cluster"
	odhtypes "github.com/opendatahub-io/opendatahub-operator/v2/pkg/controller/types"
	odhdeploy "github.com/opendatahub-io/opendatahub-operator/v2/pkg/deploy"
)

const (
	ComponentName = componentApi.DashboardComponentName

	ReadyConditionType = componentApi.DashboardKind + status.ReadySuffix

	resourceRetryInterval     = 10 * time.Second
	resourceRetryTimeout      = 1 * time.Minute
	SECRET_DEFAULT_COMPLEXITY = 16

	letterRunes = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	errEmptyAnnotation        = "secret annotations is empty"
	errNameAnnotationNotFound = "name annotation not found in secret"
	errTypeAnnotationNotFound = "type annotation not found in secret"
	errUnsupportedType        = "secret type is not supported"

	// Legacy component names are the name of the component that is assigned to deployments
	// via Kustomize. Since a deployment selector is immutable, we can't upgrade existing
	// deployment to the new component name, so keep it around till we figure out a solution.

	LegacyComponentNameUpstream   = "dashboard"
	LegacyComponentNameDownstream = "rhods-dashboard"
)

var (
	adminGroups = map[common.Platform]string{
		cluster.SelfManagedRhoai: "rhods-admins",
		cluster.ManagedRhoai:     "dedicated-admins",
		cluster.OpenDataHub:      "odh-admins",
	}

	sectionTitle = map[common.Platform]string{
		cluster.SelfManagedRhoai: "OpenShift Self Managed Services",
		cluster.ManagedRhoai:     "OpenShift Managed Services",
		cluster.OpenDataHub:      "OpenShift Open Data Hub",
	}

	baseConsoleURL = map[common.Platform]string{
		cluster.SelfManagedRhoai: "https://rhods-dashboard-",
		cluster.ManagedRhoai:     "https://rhods-dashboard-",
		cluster.OpenDataHub:      "https://odh-dashboard-",
	}

	overlaysSourcePaths = map[common.Platform]string{
		cluster.SelfManagedRhoai: "/rhoai/onprem",
		cluster.ManagedRhoai:     "/rhoai/addon",
		cluster.OpenDataHub:      "/odh",
	}

	imagesMap = map[string]string{
		"odh-dashboard-image": "RELATED_IMAGE_ODH_DASHBOARD_IMAGE",
	}

	conditionTypes = []string{
		status.ConditionDeploymentsAvailable,
	}

	secretPredicates = predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			if _, found := e.Object.GetAnnotations()[annotation.SecretNameAnnotation]; found {
				return true
			}

			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
		// this only watch for secret deletion if has with annotation
		// e.g. dashboard-oauth-client but not dashboard-oauth-client-generated
		DeleteFunc: func(e event.DeleteEvent) bool {
			if _, found := e.Object.GetAnnotations()[annotation.SecretNameAnnotation]; found {
				return true
			}

			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
	}
)

type Secret struct {
	Name             string
	Type             string
	Complexity       int
	Value            string
	OAuthClientRoute string
}

func defaultManifestInfo(p common.Platform) odhtypes.ManifestInfo {
	return odhtypes.ManifestInfo{
		Path:       odhdeploy.DefaultManifestPath,
		ContextDir: ComponentName,
		SourcePath: overlaysSourcePaths[p],
	}
}

func computeKustomizeVariable(ctx context.Context, cli client.Client, platform common.Platform, dscispec *dsciv1.DSCInitializationSpec) (map[string]string, error) {
	consoleLinkDomain, err := cluster.GetDomain(ctx, cli)
	if err != nil {
		return nil, fmt.Errorf("error getting console route URL %s : %w", consoleLinkDomain, err)
	}

	return map[string]string{
		"admin_groups":  adminGroups[platform],
		"dashboard-url": baseConsoleURL[platform] + dscispec.ApplicationsNamespace + "." + consoleLinkDomain,
		"section-title": sectionTitle[platform],
	}, nil
}

func computeComponentName() string {
	release := cluster.GetRelease()

	name := LegacyComponentNameUpstream
	if release.Name == cluster.SelfManagedRhoai || release.Name == cluster.ManagedRhoai {
		name = LegacyComponentNameDownstream
	}

	return name
}

func GetAdminGroup() string {
	return adminGroups[cluster.GetRelease().Name]
}

// getRoute returns an OpenShift route object. It waits until the .spec.host value exists to avoid possible race conditions, fails otherwise.
func getRoute(ctx context.Context, name string, namespace string, rr *odhtypes.ReconciliationRequest) (*routev1.Route, error) {
	route := &routev1.Route{}
	// Get spec.host from route
	err := wait.PollUntilContextTimeout(ctx, resourceRetryInterval, resourceRetryTimeout, false, func(ctx context.Context) (bool, error) {
		err := rr.Client.Get(ctx, client.ObjectKey{
			Name:      name,
			Namespace: namespace,
		}, route)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if route.Spec.Host == "" {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return route, err
}

func createOAuthClient(ctx context.Context, name string, secretName string, uri string, cli client.Client) error {
	log := logf.FromContext(ctx)
	// Create OAuthClient resource
	oauthClient := &oauthv1.OAuthClient{
		TypeMeta: metav1.TypeMeta{
			Kind:       "OAuthClient",
			APIVersion: "oauth.openshift.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Secret:       secretName,
		RedirectURIs: []string{"https://" + uri},
		GrantMethod:  oauthv1.GrantHandlerAuto,
	}

	err := cli.Create(ctx, oauthClient)
	if err != nil {
		if k8serr.IsAlreadyExists(err) {
			log.Info("OAuth client resource already exists, patch it", "name", oauthClient.Name)
			data, err := json.Marshal(oauthClient)
			if err != nil {
				return fmt.Errorf("failed to get DataScienceCluster custom resource data: %w", err)
			}
			if err = cli.Patch(ctx, oauthClient, client.RawPatch(types.ApplyPatchType, data),
				client.ForceOwnership, client.FieldOwner("rhods-operator")); err != nil {
				return fmt.Errorf("failed to patch existing OAuthClient CR: %w", err)
			}
			return nil
		}
	}

	return err
}

func deleteOAuthClient(ctx context.Context, secretNamespacedName types.NamespacedName, cli client.Client) error {
	oauthClient := &oauthv1.OAuthClient{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretNamespacedName.Name,
			Namespace: secretNamespacedName.Namespace,
		},
	}

	err := cli.Delete(ctx, oauthClient)
	if err != nil && !k8serr.IsNotFound(err) {
		return fmt.Errorf("error deleting OAuthClient %s: %w", oauthClient.Name, err)
	}

	return nil
}

func generateSecret(ctx context.Context, foundSecret *corev1.Secret, generatedSecret *corev1.Secret, rr *odhtypes.ReconciliationRequest) error {
	log := logf.FromContext(ctx).WithName("SecretGenerator")

	// Generate secret random value
	log.Info("Generating a random value for a secret in a namespace",
		"secret", generatedSecret.Name, "namespace", generatedSecret.Namespace)

	generatedSecret.Labels = foundSecret.Labels

	generatedSecret.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(foundSecret, foundSecret.GroupVersionKind()),
	}

	secret, err := newSecretFrom(foundSecret.GetAnnotations())
	if err != nil {
		log.Error(err, "error creating secret %s in %s", generatedSecret.Name, generatedSecret.Namespace)
		return err
	}

	generatedSecret.StringData = map[string]string{
		secret.Name: secret.Value,
	}

	err = rr.AddResources(generatedSecret)
	if err != nil {
		return err
	}

	log.Info("Done generating secret in namespace",
		"secret", generatedSecret.Name, "namespace", generatedSecret.Namespace)

	// check if annotation oauth-client-route exists
	if secret.OAuthClientRoute == "" {
		return nil
	}

	// Get OauthClient Route
	oauthClientRoute, err := getRoute(ctx, secret.OAuthClientRoute, foundSecret.Namespace, rr)
	if err != nil {
		log.Error(err, "Unable to retrieve route from OAuthClient", "route-name", secret.OAuthClientRoute)
		return err
	}

	// Generate OAuthClient for the generated secret
	log.Info("Generating an OAuthClient CR for route", "route-name", oauthClientRoute.Name)
	err = createOAuthClient(ctx, foundSecret.Name, secret.Value, oauthClientRoute.Spec.Host, rr.Client)
	if err != nil {
		log.Error(err, "error creating oauth client resource. Recreate the Secret", "secret-name",
			foundSecret.Name)

		return err
	}

	return nil
}

func newSecretFrom(annotations map[string]string) (*Secret, error) {
	// Check if annotations is not empty
	if len(annotations) == 0 {
		return nil, errors.New(errEmptyAnnotation)
	}

	var secret Secret

	// Get name from annotation
	if secretName, found := annotations[annotation.SecretNameAnnotation]; found {
		secret.Name = secretName
	} else {
		return nil, errors.New(errNameAnnotationNotFound)
	}

	// Get type from annotation
	if secretType, found := annotations[annotation.SecretTypeAnnotation]; found {
		secret.Type = secretType
	} else {
		return nil, errors.New(errTypeAnnotationNotFound)
	}

	// Get complexity from annotation
	if secretComplexity, found := annotations[annotation.SecretLengthAnnotation]; found {
		secretComplexity, err := strconv.Atoi(secretComplexity)
		if err != nil {
			return nil, err
		}
		secret.Complexity = secretComplexity
	} else {
		secret.Complexity = SECRET_DEFAULT_COMPLEXITY
	}

	if secretOAuthClientRoute, found := annotations[annotation.SecretOauthClientAnnotation]; found {
		secret.OAuthClientRoute = secretOAuthClientRoute
	}

	if err := generateSecretValue(&secret); err != nil {
		return nil, err
	}

	return &secret, nil
}

func generateSecretValue(secret *Secret) error {
	switch secret.Type {
	case "random":
		randomValue := make([]byte, secret.Complexity)
		for i := range secret.Complexity {
			num, err := rand.Int(rand.Reader, big.NewInt(int64(len(letterRunes))))
			if err != nil {
				return err
			}
			randomValue[i] = letterRunes[num.Int64()]
		}
		secret.Value = string(randomValue)
	case "oauth":
		randomValue := make([]byte, secret.Complexity)
		if _, err := rand.Read(randomValue); err != nil {
			return err
		}
		secret.Value = base64.StdEncoding.EncodeToString(
			[]byte(base64.StdEncoding.EncodeToString(randomValue)))
	default:
		return errors.New(errUnsupportedType)
	}

	return nil
}

//func newSecret(name, secretType string, complexity int) (*Secret, error) {
//	secret := &Secret{
//		Name:       name,
//		Type:       secretType,
//		Complexity: complexity,
//	}
//
//	err := generateSecretValue(secret)
//
//	return secret, err
//}
