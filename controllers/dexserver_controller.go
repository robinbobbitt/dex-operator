/*
Copyright 2021.

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

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"

	"github.com/ghodss/yaml"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/identitatem/dex-operator/api/v1alpha1"
	authv1alpha1 "github.com/identitatem/dex-operator/api/v1alpha1"
)

const (
	SECRET_MTLS_NAME      = "grpc-mtls"
	SECRET_MTLS_SUFFIX    = "-mtls"
	SECRET_WEB_TLS_SUFFIX = "-tls-secret"
	SERVICE_ACCOUNT_NAME  = "dex-operator-dexsso"
	GRPC_SERVICE_NAME     = "grpc"
	DEX_IMAGE_ENV_NAME    = "RELATED_IMAGE_DEX"
)

var (
	apiGV = authv1alpha1.GroupVersion.String()
	ctls  ClientTLS
)

type ClientTLS struct {
	caPEM            *bytes.Buffer
	clientPEM        *bytes.Buffer
	clientPrivKeyPEM *bytes.Buffer
}

// DexServerReconciler reconciles a DexServer object
type DexServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=auth.identitatem.io,resources=dexservers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=auth.identitatem.io,resources=dexservers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=auth.identitatem.io,resources=dexservers/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;patch;delete
//+kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create;patch
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources={clusterroles},verbs=get;list;watch;create;update;patch;delete;escalate;bind
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources={clusterrolebindings},verbs=get;list;create;watch
//+kubebuilder:rbac:groups="apiextensions.k8s.io",resources={customresourcedefinitions},verbs=get;list;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DexServer object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *DexServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("Reconciling...")
	dexServer := &authv1alpha1.DexServer{}
	if err := r.Get(ctx, req.NamespacedName, dexServer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch {

	case isNotDefinedMTLSSecret(dexServer, r, ctx):
		caPEM, caPrivKeyPEM, certPEM, certPrivKeyPEM, clientPEM, clientPrivKeyPEM, err := createMTLS(dexServer.Namespace)
		if err != nil {
			log.Info("failed to generate ca, cert and key")
			return ctrl.Result{}, err
		}
		spec := r.defineSecret(dexServer, caPEM, caPrivKeyPEM, certPEM, certPrivKeyPEM, clientPEM, clientPrivKeyPEM)
		log.Info("Creating a new Secret", "Secret.Namespace", spec.Namespace, "Secret.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create Secret", "Secret.Name", spec.Name)
			return ctrl.Result{}, err
		}
		ctls.caPEM = caPEM
		ctls.clientPEM = clientPEM
		ctls.clientPrivKeyPEM = clientPrivKeyPEM
		return ctrl.Result{Requeue: true}, nil

	case isNotDefinedConfigmap(dexServer, r, ctx):
		spec := r.defineConfigMap(dexServer, ctx)
		log.Info("Creating a new ConfigMap", "ConfigMap.Namespace", spec.Namespace, "ConfigMap.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create configmap", "ConfigMap.Name", spec.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case isNotDefinedService(dexServer, r, ctx):
		spec := r.defineService(dexServer)
		log.Info("Creating a new Service", "Service.Namespace", spec.Namespace, "Service.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create service", "Service.Name", spec.Name)
			return ctrl.Result{}, err
		}
		specGrpc := r.defineServiceGrpc(dexServer)
		log.Info("Creating a new Service", "Service.Namespace", specGrpc.Namespace, "Service.Name", specGrpc.Name)
		if err := r.Create(ctx, specGrpc); err != nil {
			log.Info("failed to create grpc service", "Service.Name", specGrpc.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case isNotDefinedServiceAccount(dexServer, r, ctx):
		spec := r.defineServiceAccount(dexServer)
		log.Info("Creating a new ServiceAccount", "ServiceAccount.Namespace", spec.Namespace, "ServiceAccount.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create ServiceAccount", "ServiceAccount.Name", SERVICE_ACCOUNT_NAME)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case isNotDefinedClusterRole(dexServer, r, ctx):
		spec := r.defineClusterRole(dexServer)
		log.Info("Creating a new ClusterRole", "ClusterRole.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create ClusterRole", "ClusterRole.Name", spec.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case isNotDefinedClusterRoleBinding(dexServer, r, ctx):
		spec := r.defineClusterRoleBinding(dexServer)
		log.Info("Creating a new ClusterRoleBinding", "ClusterRoleBinding.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create ClusterRoleBinding", "ClusterRoleBinding.Name", spec.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case isNotDefinedDeployment(dexServer, r, ctx):
		spec, err := r.defineDeployment(dexServer)
		if err != nil {
			log.Error(err, "Error creating deployment definition")
			return ctrl.Result{}, err
		}
		log.Info("Creating a new Deployment", "Deployment.Namespace", spec.Namespace, "Deployment.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create deployment", "Deployment.Name", spec.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case isNotDefinedRoute(dexServer, r, ctx):
		spec := r.defineRoute(dexServer)
		log.Info("Creating a new Route", "Route.Namespace", spec.Namespace, "Route.Name", spec.Name)
		if err := r.Create(ctx, spec); err != nil {
			log.Info("failed to create Route", "Route.Name", spec.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	// TODO(cdoan): check CA or CERT renew?
	default:
		log.Info("dexServer started and NOT finished")
	}

	return ctrl.Result{}, nil
}

func isNotDefinedMTLSSecret(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: fmt.Sprintf(SECRET_MTLS_NAME), Namespace: m.Namespace}, resource); err != nil && errors.IsNotFound(err) {
		return true
	}
	return false
}

func isNotDefinedRoute(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &routev1.Route{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, resource); err != nil && errors.IsNotFound(err) {
		return true
	}
	return false
}

func isNotDefinedServiceAccount(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &corev1.ServiceAccount{}
	// if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, resource); err != nil && errors.IsNotFound(err) {
	if err := r.Get(ctx, types.NamespacedName{Name: SERVICE_ACCOUNT_NAME, Namespace: m.Namespace}, resource); err != nil {
		// Sonarcloud does not like both branches returning true
		//		if errors.IsNotFound(err) {
		//			return true
		//		} else {
		//			return true
		///		}
		return true
	}
	return false
}

func isNotDefinedClusterRole(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &rbacv1.ClusterRole{}
	if err := r.Get(ctx, types.NamespacedName{Name: SERVICE_ACCOUNT_NAME}, resource); err != nil {
		return true
	}
	return false
}

func isNotDefinedClusterRoleBinding(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: SERVICE_ACCOUNT_NAME + "-" + m.Namespace}, resource); err != nil {
		return true
	}
	return false
}

func isNotDefinedDeployment(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, resource); err != nil && errors.IsNotFound(err) {
		return true
	}
	return false
}

func isNotDefinedService(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, resource); err != nil && errors.IsNotFound(err) {
		return true
	}
	return false
}

func isNotDefinedConfigmap(m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) bool {
	resource := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, resource); err != nil && errors.IsNotFound(err) {
		// spec := r.defineConfigMap(m)
		// log.Info("Creating a new ConfigMap", "ConfigMap.Namespace", spec.Namespace, "ConfigMap.Name", spec.Name)
		// if err = r.Create(ctx, spec); err != nil {
		// 	log.Debug("failed to create configmap", spec.Name)
		// 	return false
		// }
		// return true
		return true
	}
	return false
}

func getConnectorSecretFromRef(connector v1alpha1.ConnectorSpec, m *authv1alpha1.DexServer, r *DexServerReconciler, ctx context.Context) (string, error) {
	var secretNamespace, secretName string

	switch connector.Type {
	case authv1alpha1.ConnectorTypeGitHub:
		secretName = connector.GitHub.ClientSecretRef.Name
		if secretNamespace = connector.GitHub.ClientSecretRef.Namespace; secretNamespace == "" {
			secretNamespace = m.Namespace
		}
		resource := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: secretNamespace}, resource); err != nil && errors.IsNotFound(err) {
			return "", err
		}
		return string(resource.Data["clientSecret"]), nil
	case authv1alpha1.ConnectorTypeLDAP:
		secretName = connector.LDAP.BindPWRef.Name
		if secretNamespace = connector.LDAP.BindPWRef.Namespace; secretNamespace == "" {
			secretNamespace = m.Namespace
		}
		resource := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: secretNamespace}, resource); err != nil && errors.IsNotFound(err) {
			return "", err
		}
		return string(resource.Data["bindPW"]), nil
	default:
		return "", fmt.Errorf("could not retrieve secret")
	}

}

// Define the secret for grpc Mutual TLS. This secret is volume mounted on the dex instance pod. The client cert should be loaded by the gRPC client code.
func (r *DexServerReconciler) defineSecret(m *authv1alpha1.DexServer, caPEM, caPrivKeyPEM, certPEM, certPrivKeyPEM, clientPEM, clientPrivKeyPEM *bytes.Buffer) *corev1.Secret {
	labels := map[string]string{
		"app": m.Name,
	}
	secretSpec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SECRET_MTLS_NAME,
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Data: map[string][]byte{
			"ca.crt":     caPEM.Bytes(),
			"ca.key":     caPrivKeyPEM.Bytes(),
			"tls.crt":    append(certPEM.Bytes(), certPEM.Bytes()...),
			"tls.key":    certPrivKeyPEM.Bytes(),
			"client.crt": append(clientPEM.Bytes(), clientPEM.Bytes()...),
			"client.key": clientPrivKeyPEM.Bytes(),
		},
	}
	ctrl.SetControllerReference(m, secretSpec, r.Scheme)
	return secretSpec
}

func (r *DexServerReconciler) defineServiceAccount(m *authv1alpha1.DexServer) *corev1.ServiceAccount {
	labels := map[string]string{
		"app": m.Name,
	}
	serviceAccountSpec := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SERVICE_ACCOUNT_NAME,
			Namespace: m.Namespace,
			Labels:    labels,
		},
	}
	ctrl.SetControllerReference(m, serviceAccountSpec, r.Scheme)
	return serviceAccountSpec
}

func (r *DexServerReconciler) defineClusterRole(m *authv1alpha1.DexServer) *rbacv1.ClusterRole {
	clusterRoleSpec := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: SERVICE_ACCOUNT_NAME,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Resources: []string{
					"*",
				},
				Verbs: []string{
					"*",
				},
				APIGroups: []string{
					"dex.coreos.com",
				},
			},
			{
				Resources: []string{
					"customresourcedefinitions",
				},
				Verbs: []string{
					"create",
				},
				APIGroups: []string{
					"apiextensions.k8s.io",
				},
			},
		},
	}
	ctrl.SetControllerReference(m, clusterRoleSpec, r.Scheme)
	return clusterRoleSpec
}

func (r *DexServerReconciler) defineClusterRoleBinding(m *authv1alpha1.DexServer) *rbacv1.ClusterRoleBinding {
	clusterRoleBindingSpec := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: SERVICE_ACCOUNT_NAME + "-" + m.Namespace,
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     SERVICE_ACCOUNT_NAME,
			APIGroup: "rbac.authorization.k8s.io",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      SERVICE_ACCOUNT_NAME,
				Namespace: m.Namespace,
			},
		},
	}
	ctrl.SetControllerReference(m, clusterRoleBindingSpec, r.Scheme)
	return clusterRoleBindingSpec
}

func getDexImagePullSpec() (string, error) {
	imageName := os.Getenv(DEX_IMAGE_ENV_NAME)
	if len(imageName) == 0 {
		return "", fmt.Errorf("Required environment variable %v is empty or not set", DEX_IMAGE_ENV_NAME)
	}
	return imageName, nil
}

// Defines the dex instance (dex server).
func (r *DexServerReconciler) defineDeployment(m *authv1alpha1.DexServer) (*appsv1.Deployment, error) {
	ls := labelsForDexServer(m.Name, m.Namespace)
	dexImage, err := getDexImagePullSpec()
	if err != nil {
		return nil, err
	}
	// replicas := m.Spec.Size
	var replicas int32 = 1

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Command: []string{
							"/usr/local/bin/dex",
							"serve",
							"/etc/dex/cfg/config.yaml",
						},
						Image:           dexImage,
						ImagePullPolicy: corev1.PullAlways,
						Name:            m.Name,
						Env: []corev1.EnvVar{
							{
								// FIX: failed to initialize storage: failed to inspect service account token:
								//      jwt claim "kubernetes.io/serviceaccount/namespace" not found
								Name:  "KUBERNETES_POD_NAMESPACE",
								Value: m.Namespace,
							},
						},
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: 5556,
								Name:          "https",
							}, {
								ContainerPort: 5557,
								Name:          "grpc",
							},
						},
						Resources: getDexResources(m),
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "config", // the dex config.yaml
								MountPath: "/etc/dex/cfg",
							},
							{
								Name:      "tls",
								MountPath: "/etc/dex/tls",
							},
							{
								Name:      "mtls",
								MountPath: "/etc/dex/mtls",
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: m.Name,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "config.yaml",
											Path: "config.yaml",
										},
									},
								},
							},
						},
						{
							Name: "tls",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									// this secret is generated using service serving certificate via service annotation
									// service.beta.openshift.io/serving-cert-secret-name: m.Name-tls-secret
									SecretName: fmt.Sprintf(m.Name + SECRET_WEB_TLS_SUFFIX),
								},
							},
						},
						{
							Name: "mtls",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									// This secret is generated by this controller, here we load the server side cert and ca
									// service.beta.openshift.io/serving-cert-secret-name: m.Name-mtls-secret
									SecretName: SECRET_MTLS_NAME,
								},
							},
						},
					},
				},
			},
		},
	}

	// TODO: dep.Spec.Template.Spec.ServiceAccountName = m.Name
	dep.Spec.Template.Spec.ServiceAccountName = SERVICE_ACCOUNT_NAME

	ctrl.SetControllerReference(m, dep, r.Scheme)
	return dep, nil
}

func (r *DexServerReconciler) defineService(m *authv1alpha1.DexServer) *corev1.Service {
	// ls := labelsForDexConfig(m.Name)
	labels := map[string]string{
		"app": m.Name,
	}
	matchlabels := map[string]string{
		"app": m.Name,
	}
	resource := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": fmt.Sprintf(m.Name + SECRET_WEB_TLS_SUFFIX),
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Port:     5556,
					Protocol: "TCP",
					Name:     "http",
				},
			},
			Selector: matchlabels,
		},
	}
	ctrl.SetControllerReference(m, resource, r.Scheme)
	return resource
}

func (r *DexServerReconciler) defineServiceGrpc(m *authv1alpha1.DexServer) *corev1.Service {
	labels := map[string]string{
		"app": m.Name,
	}
	matchlabels := map[string]string{
		"app": m.Name,
	}
	resource := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GRPC_SERVICE_NAME,
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Port:     5557,
					Protocol: "TCP",
					Name:     "grpc",
				},
			},
			Selector: matchlabels,
		},
	}
	ctrl.SetControllerReference(m, resource, r.Scheme)
	return resource
}

// Definition of types needed to construct the config map used by the Dex application
type DexStorageConfigSpec struct {
	InCluster bool `yaml:"inCluster,omitempty"`
}

type DexStorageSpec struct {
	Type   string               `yaml:"type,omitempty"`
	Config DexStorageConfigSpec `yaml:"config,omitempty"`
}

type DexWebSpec struct {
	Http    string `yaml:"http,omitempty"`
	Https   string `yaml:"https,omitempty"`
	TlsCert string `yaml:"tlsCert,omitempty"`
	TlsKey  string `yaml:"tlsKey,omitempty"`
}

type DexGrpcSpec struct {
	Addr        string `yaml:"addr,omitempty"`
	TlsCert     string `yaml:"tlsCert,omitempty"`
	TlsKey      string `yaml:"tlsKey,omitempty"`
	TlsClientCA string `yaml:"tlsClientCA,omitempty"`
	Reflection  bool   `yaml:"reflection,omitempty"`
}

type DexConnectorConfigSpec struct {
	// Github configuration
	ClientID      string             `json:"clientID,omitempty"`
	ClientSecret  string             `json:"clientSecret,omitempty"`
	RedirectURI   string             `json:"redirectURI,omitempty"`
	Org           string             `json:"org,omitempty"`
	Orgs          []authv1alpha1.Org `json:"orgs,omitempty"`
	HostName      string             `json:"hostName,omitempty"`
	TeamNameField string             `json:"teamNameField,omitempty"`
	LoadAllGroups bool               `json:"loadAllGroups,omitempty"`
	UseLoginAsID  bool               `json:"useLoginAsID,omitempty"`

	// LDAP configuration
	Host               string                       `yaml:"host,omitempty"`
	InsecureNoSSL      bool                         `yaml:"insecureNoSSL,omitempty"`
	InsecureSkipVerify bool                         `yaml:"insecureSkipVerify,omitempty"`
	StartTLS           bool                         `yaml:"startTLS,omitempty"`
	RootCAData         []byte                       `yaml:"rootCAData,omitempty"`
	BindDN             string                       `yaml:"bindDN,omitempty"`
	BindPW             string                       `yaml:"bindPW,omitempty"`
	UsernamePrompt     string                       `yaml:"usernamePrompt,omitempty"`
	UserSearch         authv1alpha1.UserSearchSpec  `yaml:"userSearch,omitempty"`
	GroupSearch        authv1alpha1.GroupSearchSpec `yaml:"groupSearch,omitempty"`

	// Common field between GitHub and LDAP configs
	RootCA string `json:"rootCA,omitempty"`
}

type DexConnectorSpec struct {
	// +kubebuilder:validation:Enum=github;ldap
	Type   string                 `yaml:"type,omitempty"`
	Id     string                 `yaml:"id,omitempty"`
	Name   string                 `yaml:"name,omitempty"`
	Config DexConnectorConfigSpec `yaml:"config,omitempty"`
}
type DexOauth2Spec struct {
	SkipApprovalScreen bool `yaml:"skipApprovalScreen,omitempty"`
}

type DexStaticClientsSpec struct {
	Id           string   `yaml:"id,omitempty"`
	RedirectURIs []string `yaml:"redirectURIs,omitempty"`
	Name         string   `yaml:"name,omitempty"`
	Secret       string   `yaml:"secret,omitempty"`
}

type DexConfigYamlSpec struct {
	Issuer           string                 `yaml:"issuer,omitempty"`
	Storage          DexStorageSpec         `yaml:"storage,omitempty"`
	Web              DexWebSpec             `yaml:"web,omitempty"`
	Grpc             DexGrpcSpec            `yaml:"grpc,omitempty"`
	Connectors       []DexConnectorSpec     `yaml:"connectors,omitempty"`
	Oauth2           DexOauth2Spec          `yaml:"oauth2,omitempty"`
	StaticClents     []DexStaticClientsSpec `yaml:"staticClients,omitempty"`
	EnablePasswordDB bool                   `yaml:"enablePasswordDB,omitempty"`
}

func (r *DexServerReconciler) defineConfigMap(m *authv1alpha1.DexServer, ctx context.Context) *corev1.ConfigMap {
	log := ctrllog.FromContext(ctx)

	labels := map[string]string{
		"app": m.Name,
	}

	// Define config yaml data for Dex
	configYamlData := DexConfigYamlSpec{
		Issuer: m.Spec.Issuer,
		Storage: DexStorageSpec{
			Type: "kubernetes",
			Config: DexStorageConfigSpec{
				InCluster: true,
			},
		},
		Web: DexWebSpec{
			Https:   "0.0.0.0:5556",
			TlsCert: "/etc/dex/tls/tls.crt",
			TlsKey:  "/etc/dex/tls/tls.key",
		},
		Grpc: DexGrpcSpec{
			Addr:        "0.0.0.0:5557",
			TlsCert:     "/etc/dex/mtls/tls.crt",
			TlsKey:      "/etc/dex/mtls/tls.key",
			TlsClientCA: "/etc/dex/mtls/ca.crt",
			Reflection:  true,
		},
		Oauth2: DexOauth2Spec{
			SkipApprovalScreen: true,
		},
		EnablePasswordDB: true,
	}

	// Iterate over connectors defined in the DexServer to create the dex configuration for connectors
	for _, connector := range m.Spec.Connectors {
		var newConnector DexConnectorSpec

		switch connector.Type {
		case authv1alpha1.ConnectorTypeGitHub:
			// Get Github ClientSecret from SecretRef
			clientSecret, err := getConnectorSecretFromRef(connector, m, r, ctx)

			if err != nil {
				log.Error(err, "Error getting client secret")
				return nil
			}

			newConnector = DexConnectorSpec{
				Type: string(authv1alpha1.ConnectorTypeGitHub),
				Id:   connector.Id,
				Name: connector.Name,
				Config: DexConnectorConfigSpec{
					ClientID:     connector.GitHub.ClientID,
					ClientSecret: clientSecret,
					RedirectURI:  connector.GitHub.RedirectURI,
					Org:          connector.GitHub.Org,
					Orgs:         connector.GitHub.Orgs,
				},
			}
		case authv1alpha1.ConnectorTypeLDAP:
			// Get LDAP BindPW from SecretRef
			bindPW, err := getConnectorSecretFromRef(connector, m, r, ctx)

			if err != nil {
				log.Error(err, "Error getting bind pw")
				return nil
			}

			newConnector = DexConnectorSpec{
				Type: string(authv1alpha1.ConnectorTypeLDAP),
				Id:   connector.Id,
				Name: connector.Name,
				Config: DexConnectorConfigSpec{
					Host:               connector.LDAP.Host,
					InsecureNoSSL:      connector.LDAP.InsecureNoSSL,
					InsecureSkipVerify: connector.LDAP.InsecureSkipVerify,
					StartTLS:           connector.LDAP.StartTLS,
					RootCA:             connector.LDAP.RootCA,
					RootCAData:         connector.LDAP.RootCAData,
					BindDN:             connector.LDAP.BindDN,
					BindPW:             bindPW,
					UsernamePrompt:     connector.LDAP.UsernamePrompt,
				},
			}

			if connector.LDAP.UserSearch.BaseDN != "" {
				newConnector.Config.UserSearch.BaseDN = connector.LDAP.UserSearch.BaseDN
				newConnector.Config.UserSearch.Filter = connector.LDAP.UserSearch.Filter
				newConnector.Config.UserSearch.Username = connector.LDAP.UserSearch.Username
				newConnector.Config.UserSearch.Scope = connector.LDAP.UserSearch.Scope
				newConnector.Config.UserSearch.IDAttr = connector.LDAP.UserSearch.IDAttr
				newConnector.Config.UserSearch.EmailAttr = connector.LDAP.UserSearch.EmailAttr
				newConnector.Config.UserSearch.NameAttr = connector.LDAP.UserSearch.NameAttr
				newConnector.Config.UserSearch = v1alpha1.UserSearchSpec{
					BaseDN:    connector.LDAP.UserSearch.BaseDN,
					Filter:    connector.LDAP.UserSearch.Filter,
					Username:  connector.LDAP.UserSearch.Username,
					Scope:     connector.LDAP.UserSearch.Scope,
					IDAttr:    connector.LDAP.UserSearch.IDAttr,
					EmailAttr: connector.LDAP.UserSearch.EmailAttr,
					NameAttr:  connector.LDAP.UserSearch.NameAttr,
				}
			}

			if connector.LDAP.GroupSearch.BaseDN != "" {
				newConnector.Config.GroupSearch = v1alpha1.GroupSearchSpec{
					BaseDN:       connector.LDAP.GroupSearch.BaseDN,
					Filter:       connector.LDAP.GroupSearch.Filter,
					Scope:        connector.LDAP.GroupSearch.Scope,
					UserMatchers: connector.LDAP.GroupSearch.UserMatchers,
					NameAttr:     connector.LDAP.GroupSearch.NameAttr,
				}
			}

		default:
			return nil
		}

		// Add connector to list
		configYamlData.Connectors = append(configYamlData.Connectors, newConnector)
	}

	// The following code can be uncommented if we need to use StaticClients
	// Define StaticClients
	// newStaticClient := DexStaticClientsSpec{
	// 	Id:           "example-app",
	// 	RedirectURIs: []string{"http://127.0.0.1:5555/callback"},
	// 	Name:         "Example App",
	// 	Secret:       "another-client-secret",
	// }
	// configYamlData.StaticClents = append(configYamlData.StaticClents, newStaticClient)

	// Get yaml representation of configYamlData
	configYaml, err := yaml.Marshal(&configYamlData)

	if err != nil {
		log.Info("Error! failed to marshal dex config.yaml")
		return nil
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{"config.yaml": string(configYaml)},
	}
	ctrl.SetControllerReference(m, cm, r.Scheme)
	return cm
}

// https://stackoverflow.com/questions/47104454/openshift-online-v3-adding-new-route-gives-forbidden-error
func (r *DexServerReconciler) defineRoute(m *authv1alpha1.DexServer) *routev1.Route {
	ls := labelsForDexServer(m.Name, m.Namespace)
	u, _ := url.Parse(m.Spec.Issuer)
	routeHost := u.Host

	routeSpec := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels:    ls,
		},
		Spec: routev1.RouteSpec{
			Host: routeHost,
			TLS: &routev1.TLSConfig{
				// Termination: routev1.TLSTerminationPassthrough,
				// Termination: routev1.TLSTerminationEdge,
				Termination:                   routev1.TLSTerminationReencrypt,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
			},
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: m.Name,
			},
			Port: &routev1.RoutePort{
				TargetPort: intstr.IntOrString{
					Type:   intstr.String,
					StrVal: "http",
				},
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}
	ctrl.SetControllerReference(m, routeSpec, r.Scheme)
	return routeSpec
}

// getDexResources will return the ResourceRequirements for the Dex container.
func getDexResources(cr *authv1alpha1.DexServer) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{}
	return resources
}

func labelsForDexServer(name string, namespace string) map[string]string {
	return map[string]string{
		"app":                 name,
		"dexconfig_name":      name,
		"dexconfig_namespace": namespace,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *DexServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&authv1alpha1.DexServer{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.Deployment{}).
		Owns(&routev1.Route{}). /* TODO(cdoan): add Ingress */
		Complete(r)
}

// func (r *DexServerReconciler) startdexServer(ctx context.Context, ds *v1alpha1.DexServer, c client.Client) (*v1alpha1.DexServer, error) {
// 	switch {
// 	case len(ds.Spec.Connectors) != 0:
// 		log.Info("Found connector!")
// 	}
// 	return updateStatus(ctx, ds, c)
// }
