/*
Copyright 2017 The Kubernetes Authors.

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

package apimachinery

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"k8s.io/api/admissionregistration/v1beta1"
	"k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	rbacv1beta1 "k8s.io/api/rbac/v1beta1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	utilversion "k8s.io/kubernetes/pkg/util/version"
	"k8s.io/kubernetes/test/e2e/framework"
	imageutils "k8s.io/kubernetes/test/utils/image"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	_ "github.com/stretchr/testify/assert"
)

const (
	secretName      = "sample-webhook-secret"
	deploymentName  = "sample-webhook-deployment"
	serviceName     = "e2e-test-webhook"
	roleBindingName = "webhook-auth-reader"

	// The webhook configuration names should not be reused between test instances.
	crdWebhookConfigName         = "e2e-test-webhook-config-crd"
	webhookConfigName            = "e2e-test-webhook-config"
	mutatingWebhookConfigName    = "e2e-test-mutating-webhook-config"
	podMutatingWebhookConfigName = "e2e-test-mutating-webhook-pod"
	crdMutatingWebhookConfigName = "e2e-test-mutating-webhook-config-crd"
	webhookFailClosedConfigName  = "e2e-test-webhook-fail-closed"

	skipNamespaceLabelKey   = "skip-webhook-admission"
	skipNamespaceLabelValue = "yes"
	skippedNamespaceName    = "exempted-namesapce"
	disallowedPodName       = "disallowed-pod"
	hangingPodName          = "hanging-pod"
	disallowedConfigMapName = "disallowed-configmap"
	allowedConfigMapName    = "allowed-configmap"
	failNamespaceLabelKey   = "fail-closed-webhook"
	failNamespaceLabelValue = "yes"
	failNamespaceName       = "fail-closed-namesapce"
)

var serverWebhookVersion = utilversion.MustParseSemantic("v1.8.0")

var _ = SIGDescribe("AdmissionWebhook", func() {
	var context *certContext
	f := framework.NewDefaultFramework("webhook")

	var client clientset.Interface
	var namespaceName string

	BeforeEach(func() {
		client = f.ClientSet
		namespaceName = f.Namespace.Name

		// Make sure the relevant provider supports admission webhook
		framework.SkipUnlessServerVersionGTE(serverWebhookVersion, f.ClientSet.Discovery())
		framework.SkipUnlessProviderIs("gce", "gke", "local")

		_, err := f.ClientSet.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().List(metav1.ListOptions{})
		if errors.IsNotFound(err) {
			framework.Skipf("dynamic configuration of webhooks requires the admissionregistration.k8s.io group to be enabled")
		}

		By("Setting up server cert")
		context = setupServerCert(namespaceName, serviceName)
		createAuthReaderRoleBinding(f, namespaceName)

		// Note that in 1.9 we will have backwards incompatible change to
		// admission webhooks, so the image will be updated to 1.9 sometime in
		// the development 1.9 cycle.
		deployWebhookAndService(f, imageutils.GetE2EImage(imageutils.AdmissionWebhook), context)
	})

	AfterEach(func() {
		cleanWebhookTest(client, namespaceName)
	})

	It("Should be able to deny pod and configmap creation", func() {
		webhookCleanup := registerWebhook(f, context)
		defer webhookCleanup()
		testWebhook(f)
	})

	It("Should be able to deny custom resource creation", func() {
		testcrd, err := framework.CreateTestCRD(f)
		if err != nil {
			return
		}
		defer testcrd.CleanUp()
		webhookCleanup := registerWebhookForCRD(f, context, testcrd)
		defer webhookCleanup()
		testCRDWebhook(f, testcrd.Crd, testcrd.DynamicClient)
	})

	It("Should unconditionally reject operations on fail closed webhook", func() {
		webhookCleanup := registerFailClosedWebhook(f, context)
		defer webhookCleanup()
		testFailClosedWebhook(f)
	})

	It("Should mutate configmap", func() {
		webhookCleanup := registerMutatingWebhookForConfigMap(f, context)
		defer webhookCleanup()
		testMutatingConfigMapWebhook(f)
	})

	It("Should mutate pod and apply defaults after mutation", func() {
		webhookCleanup := registerMutatingWebhookForPod(f, context)
		defer webhookCleanup()
		testMutatingPodWebhook(f)
	})

	It("Should mutate crd", func() {
		testcrd, err := framework.CreateTestCRD(f)
		if err != nil {
			return
		}
		defer testcrd.CleanUp()
		webhookCleanup := registerMutatingWebhookForCRD(f, context, testcrd)
		defer webhookCleanup()
		testMutatingCRDWebhook(f, testcrd.Crd, testcrd.DynamicClient)
	})

	// TODO: add more e2e tests for mutating webhooks
	// 1. mutating webhook that mutates pod
	// 2. mutating webhook that sends empty patch
	//   2.1 and sets status.allowed=true
	//   2.2 and sets status.allowed=false
	// 3. mutating webhook that sends patch, but also sets status.allowed=false
	// 4. mtuating webhook that fail-open v.s. fail-closed
})

func createAuthReaderRoleBinding(f *framework.Framework, namespace string) {
	By("Create role binding to let webhook read extension-apiserver-authentication")
	client := f.ClientSet
	// Create the role binding to allow the webhook read the extension-apiserver-authentication configmap
	_, err := client.RbacV1beta1().RoleBindings("kube-system").Create(&rbacv1beta1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleBindingName,
			Annotations: map[string]string{
				rbacv1beta1.AutoUpdateAnnotationKey: "true",
			},
		},
		RoleRef: rbacv1beta1.RoleRef{
			APIGroup: "",
			Kind:     "Role",
			Name:     "extension-apiserver-authentication-reader",
		},
		// Webhook uses the default service account.
		Subjects: []rbacv1beta1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "default",
				Namespace: namespace,
			},
		},
	})
	if err != nil && errors.IsAlreadyExists(err) {
		framework.Logf("role binding %s already exists", roleBindingName)
	} else {
		framework.ExpectNoError(err, "creating role binding %s:webhook to access configMap", namespace)
	}
}

func deployWebhookAndService(f *framework.Framework, image string, context *certContext) {
	By("Deploying the webhook pod")
	client := f.ClientSet

	// Creating the secret that contains the webhook's cert.
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
		},
		Type: v1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": context.cert,
			"tls.key": context.key,
		},
	}
	namespace := f.Namespace.Name
	_, err := client.CoreV1().Secrets(namespace).Create(secret)
	framework.ExpectNoError(err, "creating secret %q in namespace %q", secretName, namespace)

	// Create the deployment of the webhook
	podLabels := map[string]string{"app": "sample-webhook", "webhook": "true"}
	replicas := int32(1)
	zero := int64(0)
	mounts := []v1.VolumeMount{
		{
			Name:      "webhook-certs",
			ReadOnly:  true,
			MountPath: "/webhook.local.config/certificates",
		},
	}
	volumes := []v1.Volume{
		{
			Name: "webhook-certs",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{SecretName: secretName},
			},
		},
	}
	containers := []v1.Container{
		{
			Name:         "sample-webhook",
			VolumeMounts: mounts,
			Args: []string{
				"--tls-cert-file=/webhook.local.config/certificates/tls.crt",
				"--tls-private-key-file=/webhook.local.config/certificates/tls.key",
				"--alsologtostderr",
				"-v=4",
				"2>&1",
			},
			Image: image,
		},
	}
	d := &extensions.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deploymentName,
		},
		Spec: extensions.DeploymentSpec{
			Replicas: &replicas,
			Strategy: extensions.DeploymentStrategy{
				Type: extensions.RollingUpdateDeploymentStrategyType,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: v1.PodSpec{
					TerminationGracePeriodSeconds: &zero,
					Containers:                    containers,
					Volumes:                       volumes,
				},
			},
		},
	}
	deployment, err := client.ExtensionsV1beta1().Deployments(namespace).Create(d)
	framework.ExpectNoError(err, "creating deployment %s in namespace %s", deploymentName, namespace)
	By("Wait for the deployment to be ready")
	err = framework.WaitForDeploymentRevisionAndImage(client, namespace, deploymentName, "1", image)
	framework.ExpectNoError(err, "waiting for the deployment of image %s in %s in %s to complete", image, deploymentName, namespace)
	err = framework.WaitForDeploymentComplete(client, deployment)
	framework.ExpectNoError(err, "waiting for the deployment status valid", image, deploymentName, namespace)

	By("Deploying the webhook service")

	serviceLabels := map[string]string{"webhook": "true"}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      serviceName,
			Labels:    map[string]string{"test": "webhook"},
		},
		Spec: v1.ServiceSpec{
			Selector: serviceLabels,
			Ports: []v1.ServicePort{
				{
					Protocol:   "TCP",
					Port:       443,
					TargetPort: intstr.FromInt(443),
				},
			},
		},
	}
	_, err = client.CoreV1().Services(namespace).Create(service)
	framework.ExpectNoError(err, "creating service %s in namespace %s", serviceName, namespace)

	By("Verifying the service has paired with the endpoint")
	err = framework.WaitForServiceEndpointsNum(client, namespace, serviceName, 1, 1*time.Second, 30*time.Second)
	framework.ExpectNoError(err, "waiting for service %s/%s have %d endpoint", namespace, serviceName, 1)
}

func strPtr(s string) *string { return &s }

func registerWebhook(f *framework.Framework, context *certContext) func() {
	client := f.ClientSet
	By("Registering the webhook via the AdmissionRegistration API")

	namespace := f.Namespace.Name
	configName := webhookConfigName
	// A webhook that cannot talk to server, with fail-open policy
	failOpenHook := failingWebhook(namespace, "fail-open.k8s.io")
	policyIgnore := v1beta1.Ignore
	failOpenHook.FailurePolicy = &policyIgnore

	_, err := client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Create(&v1beta1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: configName,
		},
		Webhooks: []v1beta1.Webhook{
			{
				Name: "deny-unwanted-pod-container-name-and-label.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create},
					Rule: v1beta1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"pods"},
					},
				}},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/pods"),
					},
					CABundle: context.signingCert,
				},
			},
			{
				Name: "deny-unwanted-configmap-data.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create, v1beta1.Update},
					Rule: v1beta1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"configmaps"},
					},
				}},
				// The webhook skips the namespace that has label "skip-webhook-admission":"yes"
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      skipNamespaceLabelKey,
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{skipNamespaceLabelValue},
						},
					},
				},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/configmaps"),
					},
					CABundle: context.signingCert,
				},
			},
			// Server cannot talk to this webhook, so it always fails.
			// Because this webhook is configured fail-open, request should be admitted after the call fails.
			failOpenHook,
		},
	})
	framework.ExpectNoError(err, "registering webhook config %s with namespace %s", configName, namespace)

	// The webhook configuration is honored in 1s.
	time.Sleep(10 * time.Second)

	return func() {
		client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Delete(configName, nil)
	}
}

func registerMutatingWebhookForConfigMap(f *framework.Framework, context *certContext) func() {
	client := f.ClientSet
	By("Registering the mutating configmap webhook via the AdmissionRegistration API")

	namespace := f.Namespace.Name
	configName := mutatingWebhookConfigName

	_, err := client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Create(&v1beta1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: configName,
		},
		Webhooks: []v1beta1.Webhook{
			{
				Name: "adding-configmap-data-stage-1.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create},
					Rule: v1beta1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"configmaps"},
					},
				}},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/mutating-configmaps"),
					},
					CABundle: context.signingCert,
				},
			},
			{
				Name: "adding-configmap-data-stage-2.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create},
					Rule: v1beta1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"configmaps"},
					},
				}},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/mutating-configmaps"),
					},
					CABundle: context.signingCert,
				},
			},
		},
	})
	framework.ExpectNoError(err, "registering mutating webhook config %s with namespace %s", configName, namespace)

	// The webhook configuration is honored in 1s.
	time.Sleep(10 * time.Second)
	return func() { client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Delete(configName, nil) }
}

func testMutatingConfigMapWebhook(f *framework.Framework) {
	By("create a configmap that should be updated by the webhook")
	client := f.ClientSet
	configMap := toBeMutatedConfigMap(f)
	mutatedConfigMap, err := client.CoreV1().ConfigMaps(f.Namespace.Name).Create(configMap)
	Expect(err).To(BeNil())
	expectedConfigMapData := map[string]string{
		"mutation-start":   "yes",
		"mutation-stage-1": "yes",
		"mutation-stage-2": "yes",
	}
	if !reflect.DeepEqual(expectedConfigMapData, mutatedConfigMap.Data) {
		framework.Failf("\nexpected %#v\n, got %#v\n", expectedConfigMapData, mutatedConfigMap.Data)
	}
}

func registerMutatingWebhookForPod(f *framework.Framework, context *certContext) func() {
	client := f.ClientSet
	By("Registering the mutating pod webhook via the AdmissionRegistration API")

	namespace := f.Namespace.Name
	configName := podMutatingWebhookConfigName

	_, err := client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Create(&v1beta1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: configName,
		},
		Webhooks: []v1beta1.Webhook{
			{
				Name: "adding-init-container.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create},
					Rule: v1beta1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"pods"},
					},
				}},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/mutating-pods"),
					},
					CABundle: context.signingCert,
				},
			},
		},
	})
	framework.ExpectNoError(err, "registering mutating webhook config %s with namespace %s", configName, namespace)

	// The webhook configuration is honored in 1s.
	time.Sleep(10 * time.Second)

	return func() { client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Delete(configName, nil) }
}

func testMutatingPodWebhook(f *framework.Framework) {
	By("create a pod that should be updated by the webhook")
	client := f.ClientSet
	configMap := toBeMutatedPod(f)
	mutatedPod, err := client.CoreV1().Pods(f.Namespace.Name).Create(configMap)
	Expect(err).To(BeNil())
	if len(mutatedPod.Spec.InitContainers) != 1 {
		framework.Failf("expect pod to have 1 init container, got %#v", mutatedPod.Spec.InitContainers)
	}
	if got, expected := mutatedPod.Spec.InitContainers[0].Name, "webhook-added-init-container"; got != expected {
		framework.Failf("expect the init container name to be %q, got %q", expected, got)
	}
	if got, expected := mutatedPod.Spec.InitContainers[0].TerminationMessagePolicy, v1.TerminationMessageReadFile; got != expected {
		framework.Failf("expect the init terminationMessagePolicy to be default to %q, got %q", expected, got)
	}
}

func toBeMutatedPod(f *framework.Framework) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "webhook-to-be-mutated",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "example",
					Image: framework.GetPauseImageName(f.ClientSet),
				},
			},
		},
	}
}

func testWebhook(f *framework.Framework) {
	By("create a pod that should be denied by the webhook")
	client := f.ClientSet
	// Creating the pod, the request should be rejected
	pod := nonCompliantPod(f)
	_, err := client.CoreV1().Pods(f.Namespace.Name).Create(pod)
	Expect(err).NotTo(BeNil())
	expectedErrMsg1 := "the pod contains unwanted container name"
	if !strings.Contains(err.Error(), expectedErrMsg1) {
		framework.Failf("expect error contains %q, got %q", expectedErrMsg1, err.Error())
	}
	expectedErrMsg2 := "the pod contains unwanted label"
	if !strings.Contains(err.Error(), expectedErrMsg2) {
		framework.Failf("expect error contains %q, got %q", expectedErrMsg2, err.Error())
	}

	By("create a pod that causes the webhook to hang")
	client = f.ClientSet
	// Creating the pod, the request should be rejected
	pod = hangingPod(f)
	_, err = client.CoreV1().Pods(f.Namespace.Name).Create(pod)
	Expect(err).NotTo(BeNil())
	expectedTimeoutErr := "request did not complete within allowed duration"
	if !strings.Contains(err.Error(), expectedTimeoutErr) {
		framework.Failf("expect timeout error %q, got %q", expectedTimeoutErr, err.Error())
	}

	By("create a configmap that should be denied by the webhook")
	// Creating the configmap, the request should be rejected
	configmap := nonCompliantConfigMap(f)
	_, err = client.CoreV1().ConfigMaps(f.Namespace.Name).Create(configmap)
	Expect(err).NotTo(BeNil())
	expectedErrMsg := "the configmap contains unwanted key and value"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		framework.Failf("expect error contains %q, got %q", expectedErrMsg, err.Error())
	}

	By("create a configmap that should be admitted by the webhook")
	// Creating the configmap, the request should be admitted
	configmap = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: allowedConfigMapName,
		},
		Data: map[string]string{
			"admit": "this",
		},
	}
	_, err = client.CoreV1().ConfigMaps(f.Namespace.Name).Create(configmap)
	Expect(err).NotTo(HaveOccurred())

	By("update (PUT) the admitted configmap to a non-compliant one should be rejected by the webhook")
	toNonCompliantFn := func(cm *v1.ConfigMap) {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["webhook-e2e-test"] = "webhook-disallow"
	}
	_, err = updateConfigMap(client, f.Namespace.Name, allowedConfigMapName, toNonCompliantFn)
	Expect(err).NotTo(BeNil())
	if !strings.Contains(err.Error(), expectedErrMsg) {
		framework.Failf("expect error contains %q, got %q", expectedErrMsg, err.Error())
	}

	By("update (PATCH) the admitted configmap to a non-compliant one should be rejected by the webhook")
	patch := nonCompliantConfigMapPatch()
	_, err = client.CoreV1().ConfigMaps(f.Namespace.Name).Patch(allowedConfigMapName, types.StrategicMergePatchType, []byte(patch))
	Expect(err).NotTo(BeNil())
	if !strings.Contains(err.Error(), expectedErrMsg) {
		framework.Failf("expect error contains %q, got %q", expectedErrMsg, err.Error())
	}

	By("create a namespace that bypass the webhook")
	err = createNamespace(f, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: skippedNamespaceName,
		Labels: map[string]string{
			skipNamespaceLabelKey: skipNamespaceLabelValue,
		},
	}})
	framework.ExpectNoError(err, "creating namespace %q", skippedNamespaceName)
	// clean up the namespace
	defer client.CoreV1().Namespaces().Delete(skippedNamespaceName, nil)

	By("create a configmap that violates the webhook policy but is in a whitelisted namespace")
	configmap = nonCompliantConfigMap(f)
	_, err = client.CoreV1().ConfigMaps(skippedNamespaceName).Create(configmap)
	Expect(err).To(BeNil())
}

// failingWebhook returns a webhook with rule of create configmaps,
// but with an invalid client config so that server cannot communicate with it
func failingWebhook(namespace, name string) v1beta1.Webhook {
	return v1beta1.Webhook{
		Name: name,
		Rules: []v1beta1.RuleWithOperations{{
			Operations: []v1beta1.OperationType{v1beta1.Create},
			Rule: v1beta1.Rule{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"configmaps"},
			},
		}},
		ClientConfig: v1beta1.WebhookClientConfig{
			Service: &v1beta1.ServiceReference{
				Namespace: namespace,
				Name:      serviceName,
				Path:      strPtr("/configmaps"),
			},
			// Without CA bundle, the call to webhook always fails
			CABundle: nil,
		},
	}
}

func registerFailClosedWebhook(f *framework.Framework, context *certContext) func() {
	client := f.ClientSet
	By("Registering a webhook that server cannot talk to, with fail closed policy, via the AdmissionRegistration API")

	namespace := f.Namespace.Name
	configName := webhookFailClosedConfigName
	// A webhook that cannot talk to server, with fail-closed policy
	policyFail := v1beta1.Fail
	hook := failingWebhook(namespace, "fail-closed.k8s.io")
	hook.FailurePolicy = &policyFail
	hook.NamespaceSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      failNamespaceLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{failNamespaceLabelValue},
			},
		},
	}

	_, err := client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Create(&v1beta1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: configName,
		},
		Webhooks: []v1beta1.Webhook{
			// Server cannot talk to this webhook, so it always fails.
			// Because this webhook is configured fail-closed, request should be rejected after the call fails.
			hook,
		},
	})
	framework.ExpectNoError(err, "registering webhook config %s with namespace %s", configName, namespace)

	// The webhook configuration is honored in 10s.
	time.Sleep(10 * time.Second)
	return func() {
		f.ClientSet.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Delete(configName, nil)
	}
}

func testFailClosedWebhook(f *framework.Framework) {
	client := f.ClientSet
	By("create a namespace for the webhook")
	err := createNamespace(f, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: failNamespaceName,
		Labels: map[string]string{
			failNamespaceLabelKey: failNamespaceLabelValue,
		},
	}})
	framework.ExpectNoError(err, "creating namespace %q", failNamespaceName)
	defer client.CoreV1().Namespaces().Delete(failNamespaceName, nil)

	By("create a configmap should be unconditionally rejected by the webhook")
	configmap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
	}
	_, err = client.CoreV1().ConfigMaps(failNamespaceName).Create(configmap)
	Expect(err).To(HaveOccurred())
	if !errors.IsInternalError(err) {
		framework.Failf("expect an internal error, got %#v", err)
	}
}

func createNamespace(f *framework.Framework, ns *v1.Namespace) error {
	return wait.PollImmediate(100*time.Millisecond, 30*time.Second, func() (bool, error) {
		_, err := f.ClientSet.CoreV1().Namespaces().Create(ns)
		if err != nil {
			if strings.HasPrefix(err.Error(), "object is being deleted:") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func nonCompliantPod(f *framework.Framework) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: disallowedPodName,
			Labels: map[string]string{
				"webhook-e2e-test": "webhook-disallow",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "webhook-disallow",
					Image: framework.GetPauseImageName(f.ClientSet),
				},
			},
		},
	}
}

func hangingPod(f *framework.Framework) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: hangingPodName,
			Labels: map[string]string{
				"webhook-e2e-test": "wait-forever",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "wait-forever",
					Image: framework.GetPauseImageName(f.ClientSet),
				},
			},
		},
	}
}

func nonCompliantConfigMap(f *framework.Framework) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: disallowedConfigMapName,
		},
		Data: map[string]string{
			"webhook-e2e-test": "webhook-disallow",
		},
	}
}

func toBeMutatedConfigMap(f *framework.Framework) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "to-be-mutated",
		},
		Data: map[string]string{
			"mutation-start": "yes",
		},
	}
}

func nonCompliantConfigMapPatch() string {
	return fmt.Sprint(`{"data":{"webhook-e2e-test":"webhook-disallow"}}`)
}

type updateConfigMapFn func(cm *v1.ConfigMap)

func updateConfigMap(c clientset.Interface, ns, name string, update updateConfigMapFn) (*v1.ConfigMap, error) {
	var cm *v1.ConfigMap
	pollErr := wait.PollImmediate(2*time.Second, 1*time.Minute, func() (bool, error) {
		var err error
		if cm, err = c.CoreV1().ConfigMaps(ns).Get(name, metav1.GetOptions{}); err != nil {
			return false, err
		}
		update(cm)
		if cm, err = c.CoreV1().ConfigMaps(ns).Update(cm); err == nil {
			return true, nil
		}
		// Only retry update on conflict
		if !errors.IsConflict(err) {
			return false, err
		}
		return false, nil
	})
	return cm, pollErr
}

func cleanWebhookTest(client clientset.Interface, namespaceName string) {
	_ = client.CoreV1().Services(namespaceName).Delete(serviceName, nil)
	_ = client.ExtensionsV1beta1().Deployments(namespaceName).Delete(deploymentName, nil)
	_ = client.CoreV1().Secrets(namespaceName).Delete(secretName, nil)
	_ = client.RbacV1beta1().RoleBindings("kube-system").Delete(roleBindingName, nil)
}

func registerWebhookForCRD(f *framework.Framework, context *certContext, testcrd *framework.TestCrd) func() {
	client := f.ClientSet
	By("Registering the crd webhook via the AdmissionRegistration API")

	namespace := f.Namespace.Name
	configName := crdWebhookConfigName
	_, err := client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Create(&v1beta1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: configName,
		},
		Webhooks: []v1beta1.Webhook{
			{
				Name: "deny-unwanted-crd-data.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create},
					Rule: v1beta1.Rule{
						APIGroups:   []string{testcrd.ApiGroup},
						APIVersions: []string{testcrd.ApiVersion},
						Resources:   []string{testcrd.GetPluralName()},
					},
				}},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/crd"),
					},
					CABundle: context.signingCert,
				},
			},
		},
	})
	framework.ExpectNoError(err, "registering crd webhook config %s with namespace %s", configName, namespace)

	// The webhook configuration is honored in 1s.
	time.Sleep(10 * time.Second)
	return func() {
		client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Delete(configName, nil)
	}
}

func registerMutatingWebhookForCRD(f *framework.Framework, context *certContext, testcrd *framework.TestCrd) func() {
	client := f.ClientSet
	By("Registering the mutating webhook for crd via the AdmissionRegistration API")

	namespace := f.Namespace.Name
	configName := crdMutatingWebhookConfigName
	_, err := client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Create(&v1beta1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: configName,
		},
		Webhooks: []v1beta1.Webhook{
			{
				Name: "mutate-crd-data-stage-1.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create},
					Rule: v1beta1.Rule{
						APIGroups:   []string{testcrd.ApiGroup},
						APIVersions: []string{testcrd.ApiVersion},
						Resources:   []string{testcrd.GetPluralName()},
					},
				}},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/mutating-crd"),
					},
					CABundle: context.signingCert,
				},
			},
			{
				Name: "mutate-crd-data-stage-2.k8s.io",
				Rules: []v1beta1.RuleWithOperations{{
					Operations: []v1beta1.OperationType{v1beta1.Create},
					Rule: v1beta1.Rule{
						APIGroups:   []string{testcrd.ApiGroup},
						APIVersions: []string{testcrd.ApiVersion},
						Resources:   []string{testcrd.GetPluralName()},
					},
				}},
				ClientConfig: v1beta1.WebhookClientConfig{
					Service: &v1beta1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      strPtr("/mutating-crd"),
					},
					CABundle: context.signingCert,
				},
			},
		},
	})
	framework.ExpectNoError(err, "registering crd webhook config %s with namespace %s", configName, namespace)

	// The webhook configuration is honored in 1s.
	time.Sleep(10 * time.Second)

	return func() { client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Delete(configName, nil) }
}

func testCRDWebhook(f *framework.Framework, crd *apiextensionsv1beta1.CustomResourceDefinition, crdClient dynamic.ResourceInterface) {
	By("Creating a custom resource that should be denied by the webhook")
	crInstance := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       crd.Spec.Names.Kind,
			"apiVersion": crd.Spec.Group + "/" + crd.Spec.Version,
			"metadata": map[string]interface{}{
				"name":      "cr-instance-1",
				"namespace": f.Namespace.Name,
			},
			"data": map[string]interface{}{
				"webhook-e2e-test": "webhook-disallow",
			},
		},
	}
	_, err := crdClient.Create(crInstance)
	Expect(err).NotTo(BeNil())
	expectedErrMsg := "the custom resource contains unwanted data"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		framework.Failf("expect error contains %q, got %q", expectedErrMsg, err.Error())
	}
}

func testMutatingCRDWebhook(f *framework.Framework, crd *apiextensionsv1beta1.CustomResourceDefinition, crdClient dynamic.ResourceInterface) {
	By("Creating a custom resource that should be mutated by the webhook")
	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       crd.Spec.Names.Kind,
			"apiVersion": crd.Spec.Group + "/" + crd.Spec.Version,
			"metadata": map[string]interface{}{
				"name":      "cr-instance-1",
				"namespace": f.Namespace.Name,
			},
			"data": map[string]interface{}{
				"mutation-start": "yes",
			},
		},
	}
	mutatedCR, err := crdClient.Create(cr)
	Expect(err).To(BeNil())
	expectedCRData := map[string]interface{}{
		"mutation-start":   "yes",
		"mutation-stage-1": "yes",
		"mutation-stage-2": "yes",
	}
	if !reflect.DeepEqual(expectedCRData, mutatedCR.Object["data"]) {
		framework.Failf("\nexpected %#v\n, got %#v\n", expectedCRData, mutatedCR.Object["data"])
	}
}