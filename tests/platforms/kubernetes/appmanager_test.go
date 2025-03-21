/*
Copyright 2021 The Dapr Authors
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

package kubernetes

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
)

const (
	testNamespace = "apputil-test"
	getVerb       = "get"
	createVerb    = "create"
	updateVerb    = "update"
)

func newDefaultFakeClient() *KubeClient {
	fakeclient := fake.NewSimpleClientset()
	return &KubeClient{
		ClientSet: fakeclient,
	}
}

func newFakeKubeClient() *KubeClient {
	return &KubeClient{
		ClientSet: &fake.Clientset{},
	}
}

func testAppDescription() AppDescription {
	return AppDescription{
		AppName:        "testapp",
		DaprEnabled:    true,
		ImageName:      "helloworld",
		RegistryName:   "dapriotest",
		Replicas:       1,
		IngressEnabled: true,
		MetricsEnabled: true,
	}
}

func TestDeployApp(t *testing.T) {
	client := newDefaultFakeClient()
	testApp := testAppDescription()
	appManager := NewAppManager(client, testNamespace, testApp)

	// act
	_, err := appManager.Deploy()
	require.NoError(t, err)

	// assert
	deploymentClient := client.Deployments(testNamespace)
	deployment, _ := deploymentClient.Get(t.Context(), testApp.AppName, metav1.GetOptions{})
	assert.NotNil(t, deployment)
	assert.Equal(t, testApp.AppName, deployment.ObjectMeta.Name)
	assert.Equal(t, testNamespace, deployment.ObjectMeta.Namespace)
	assert.Equal(t, int32(1), *deployment.Spec.Replicas)
	assert.Equal(t, testApp.AppName, deployment.Spec.Selector.MatchLabels["testapp"])
	assert.Equal(t, "true", deployment.Spec.Template.ObjectMeta.Annotations["dapr.io/enabled"])
	assert.Equal(t, testApp.AppName, deployment.Spec.Template.Spec.Containers[0].Name)
	assert.Equal(t, "dapriotest/helloworld", deployment.Spec.Template.Spec.Containers[0].Image)
}

func TestWaitUntilDeploymentState(t *testing.T) {
	testApp := testAppDescription()
	var createdDeploymentObj *appsv1.Deployment

	t.Run("deployment is in done state", func(t *testing.T) {
		client := newFakeKubeClient()
		getVerbCalled := 0
		const expectedGetVerbCalled = 2

		// Set up reactor to fake verb
		client.ClientSet.(*fake.Clientset).AddReactor(
			"*",
			"deployments",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)

				switch action.GetVerb() {
				case createVerb:
					// return the same deployment object
					createdDeploymentObj = action.(core.CreateAction).GetObject().(*appsv1.Deployment)
					createdDeploymentObj.Status.ReadyReplicas = 0
					createdDeploymentObj.Status.AvailableReplicas = 0

				case getVerb:
					// set Replicas to the target replicas when WaitUntilDeploymentState called
					// get verb 'expectedGetVerbCalled' times
					if getVerbCalled == expectedGetVerbCalled {
						createdDeploymentObj.Status.ReadyReplicas = testApp.Replicas
						createdDeploymentObj.Status.AvailableReplicas = testApp.Replicas
					} else {
						getVerbCalled++
					}
				}
				return true, createdDeploymentObj, nil
			})

		appManager := NewAppManager(client, testNamespace, testApp)

		// act
		_, err := appManager.Deploy()
		require.NoError(t, err)

		// assert
		d, err := appManager.WaitUntilDeploymentState(appManager.IsDeploymentDone)

		require.NoError(t, err)
		assert.Equal(t, testApp.Replicas, d.Status.ReadyReplicas)
		assert.Equal(t, expectedGetVerbCalled, getVerbCalled)
	})

	t.Run("deployment is in deleted state", func(t *testing.T) {
		client := newFakeKubeClient()
		getVerbCalled := 0
		const expectedGetVerbCalled = 2

		// Set up reactor to fake verb
		client.ClientSet.(*fake.Clientset).AddReactor(
			"*",
			"deployments",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)

				switch action.GetVerb() {
				case createVerb:
					// return the same deployment object
					createdDeploymentObj = action.(core.CreateAction).GetObject().(*appsv1.Deployment)
					createdDeploymentObj.Status.Replicas = testApp.Replicas

				case getVerb:
					// return notfound error when WaitUntilDeploymentState called
					// get verb 'expectedGetVerbCalled' times
					if getVerbCalled == expectedGetVerbCalled {
						err := errors.NewNotFound(
							schema.GroupResource{
								Group:    "fakeGroup",
								Resource: "fakeResource",
							},
							"deployments")

						return true, nil, err
					}

					getVerbCalled++
				}

				return true, createdDeploymentObj, nil
			})

		appManager := NewAppManager(client, testNamespace, testApp)

		// act
		_, err := appManager.Deploy()
		require.NoError(t, err)

		// assert
		d, err := appManager.WaitUntilDeploymentState(appManager.IsDeploymentDeleted)

		require.NoError(t, err)
		assert.Equal(t, new(appsv1.Deployment), d)
		assert.Equal(t, expectedGetVerbCalled, getVerbCalled)
	})
}

func TestScaleDeploymentReplica(t *testing.T) {
	testApp := testAppDescription()
	client := newFakeKubeClient()
	// Set up reactor to fake verb
	client.ClientSet.(*fake.Clientset).AddReactor(
		"*",
		"deployments",
		func(action core.Action) (bool, runtime.Object, error) {
			ns := action.GetNamespace()
			assert.Equal(t, testNamespace, ns)
			subRs := action.GetSubresource()
			assert.Equal(t, "scale", subRs)

			var scaleObj *autoscalingv1.Scale

			switch action.GetVerb() {
			case getVerb:
				scaleObj = &autoscalingv1.Scale{
					Status: autoscalingv1.ScaleStatus{
						Replicas: 1,
					},
				}

			case updateVerb:
				scaleObj = &autoscalingv1.Scale{
					Status: autoscalingv1.ScaleStatus{
						Replicas: 3,
					},
				}
			}

			return true, scaleObj, nil
		})

	appManager := NewAppManager(client, testNamespace, testApp)

	t.Run("lower bound check", func(t *testing.T) {
		err := appManager.ScaleDeploymentReplica(-1)
		require.Error(t, err)
	})

	t.Run("upper bound check", func(t *testing.T) {
		err := appManager.ScaleDeploymentReplica(maxReplicas + 1)
		require.Error(t, err)
	})

	t.Run("same replicas", func(t *testing.T) {
		err := appManager.ScaleDeploymentReplica(1)
		require.NoError(t, err)
	})

	t.Run("new replicas", func(t *testing.T) {
		err := appManager.ScaleDeploymentReplica(3)
		require.NoError(t, err)
	})
}

func TestValidateSidecar(t *testing.T) {
	testApp := testAppDescription()

	objMeta := metav1.ObjectMeta{
		Name:      testApp.AppName,
		Namespace: testNamespace,
		Labels: map[string]string{
			TestAppLabelKey: testApp.AppName,
		},
	}

	t.Run("Sidecar is injected", func(t *testing.T) {
		client := newFakeKubeClient()
		// Set up reactor to fake verb
		client.ClientSet.(*fake.Clientset).AddReactor(
			"list",
			"pods",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)

				singlePod := apiv1.Pod{
					ObjectMeta: objMeta,
					Spec: apiv1.PodSpec{
						Containers: []apiv1.Container{
							{
								Name:  "daprd",
								Image: "daprio/daprd:latest",
							},
							{
								Name:  testApp.AppName,
								Image: fmt.Sprintf("%s/%s", testApp.RegistryName, testApp.ImageName),
							},
						},
					},
				}

				podList := &apiv1.PodList{
					Items: []apiv1.Pod{singlePod},
				}

				return true, podList, nil
			})

		appManager := NewAppManager(client, testNamespace, testApp)
		err := appManager.ValidateSidecar()

		require.NoError(t, err)
	})

	t.Run("Sidecar is not injected", func(t *testing.T) {
		client := newFakeKubeClient()
		// Set up reactor to fake verb
		client.ClientSet.(*fake.Clientset).AddReactor(
			"list",
			"pods",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)

				singlePod := apiv1.Pod{
					ObjectMeta: objMeta,
					Spec: apiv1.PodSpec{
						Containers: []apiv1.Container{
							{
								Name:  testApp.AppName,
								Image: fmt.Sprintf("%s/%s", testApp.RegistryName, testApp.ImageName),
							},
						},
					},
				}

				podList := &apiv1.PodList{
					Items: []apiv1.Pod{singlePod},
				}

				return true, podList, nil
			})

		appManager := NewAppManager(client, testNamespace, testApp)
		err := appManager.ValidateSidecar()
		require.Error(t, err)
	})

	t.Run("Pod is not found", func(t *testing.T) {
		client := newFakeKubeClient()
		// Set up reactor to fake verb
		client.ClientSet.(*fake.Clientset).AddReactor(
			"list",
			"pods",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)

				podList := &apiv1.PodList{
					Items: []apiv1.Pod{},
				}

				return true, podList, nil
			})

		appManager := NewAppManager(client, testNamespace, testApp)
		err := appManager.ValidateSidecar()
		require.Error(t, err)
	})
}

func TestCreateIngressService(t *testing.T) {
	testApp := testAppDescription()

	t.Run("Ingress is disabled", func(t *testing.T) {
		client := newDefaultFakeClient()
		testApp.IngressEnabled = false
		appManager := NewAppManager(client, testNamespace, testApp)

		_, err := appManager.CreateIngressService()
		require.NoError(t, err)
		// assert
		serviceClient := client.Services(testNamespace)
		obj, _ := serviceClient.Get(t.Context(), testApp.AppName, metav1.GetOptions{})
		assert.NotNil(t, obj)
		assert.Equal(t, testApp.AppName, obj.ObjectMeta.Name)
		assert.Equal(t, testNamespace, obj.ObjectMeta.Namespace)
		assert.Equal(t, apiv1.ServiceTypeClusterIP, obj.Spec.Type)
	})

	t.Run("Ingress is enabled", func(t *testing.T) {
		client := newDefaultFakeClient()
		testApp.IngressEnabled = true
		appManager := NewAppManager(client, testNamespace, testApp)

		_, err := appManager.CreateIngressService()
		require.NoError(t, err)
		// assert
		serviceClient := client.Services(testNamespace)
		obj, _ := serviceClient.Get(t.Context(), testApp.AppName, metav1.GetOptions{})
		assert.NotNil(t, obj)
		assert.Equal(t, testApp.AppName, obj.ObjectMeta.Name)
		assert.Equal(t, testNamespace, obj.ObjectMeta.Namespace)
		assert.Equal(t, apiv1.ServiceTypeLoadBalancer, obj.Spec.Type)
	})
}

func TestWaitUntilServiceStateAndGetExternalURL(t *testing.T) {
	// fake test values
	fakeMinikubeNodeIP := "192.168.0.12"
	fakeNodePort := int32(3000)
	fakeExternalIP := "10.10.10.100"
	testApp := testAppDescription()

	t.Run("Minikube environment", func(t *testing.T) {
		t.Setenv(MiniKubeIPEnvVar, fakeMinikubeNodeIP)

		client := newFakeKubeClient()
		// Set up reactor to fake verb
		client.ClientSet.(*fake.Clientset).AddReactor(
			getVerb,
			"services",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)
				obj := &apiv1.Service{
					Spec: apiv1.ServiceSpec{
						Ports: []apiv1.ServicePort{
							{
								NodePort: fakeNodePort,
							},
						},
					},
				}
				return true, obj, nil
			})

		appManager := NewAppManager(client, testNamespace, testApp)
		svcObj, err := appManager.WaitUntilServiceState(appManager.app.AppName, appManager.IsServiceIngressReady)
		require.NoError(t, err)

		externalURL := appManager.AcquireExternalURLFromService(svcObj)
		assert.Equal(t, externalURL, fmt.Sprintf("%s:%d", fakeMinikubeNodeIP, fakeNodePort))
	})

	t.Run("Kubernetes environment", func(t *testing.T) {
		getVerbCalled := 0
		const expectedGetVerbCalled = 2
		t.Setenv(MiniKubeIPEnvVar, "")

		client := newFakeKubeClient()
		// Set up reactor to fake verb
		client.ClientSet.(*fake.Clientset).AddReactor(
			getVerb,
			"services",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)

				obj := &apiv1.Service{
					Spec: apiv1.ServiceSpec{
						Ports: []apiv1.ServicePort{},
					},
					Status: apiv1.ServiceStatus{
						LoadBalancer: apiv1.LoadBalancerStatus{
							Ingress: []apiv1.LoadBalancerIngress{},
						},
					},
				}

				if getVerbCalled == expectedGetVerbCalled {
					obj.Status.LoadBalancer.Ingress = []apiv1.LoadBalancerIngress{
						{
							IP: fakeExternalIP,
						},
					}
					obj.Spec.Ports = []apiv1.ServicePort{
						{
							Port: fakeNodePort,
						},
					}
				} else {
					getVerbCalled++
				}

				return true, obj, nil
			})

		appManager := NewAppManager(client, testNamespace, testApp)
		svcObj, err := appManager.WaitUntilServiceState(appManager.app.AppName, appManager.IsServiceIngressReady)
		require.NoError(t, err)

		externalURL := appManager.AcquireExternalURLFromService(svcObj)
		assert.Equal(t, fmt.Sprintf("%s:%d", fakeExternalIP, fakeNodePort), externalURL)
		assert.Equal(t, expectedGetVerbCalled, getVerbCalled)
	})
}

func TestWaitUntilServiceStateDeleted(t *testing.T) {
	// fake test values
	testApp := testAppDescription()
	client := newFakeKubeClient()
	// Set up reactor to fake verb
	client.ClientSet.(*fake.Clientset).AddReactor(
		getVerb,
		"services",
		func(action core.Action) (bool, runtime.Object, error) {
			ns := action.GetNamespace()
			assert.Equal(t, testNamespace, ns)
			err := errors.NewNotFound(
				schema.GroupResource{
					Group:    "fakeGroup",
					Resource: "fakeResource",
				},
				"services")

			return true, nil, err
		})

	appManager := NewAppManager(client, testNamespace, testApp)
	svcObj, err := appManager.WaitUntilServiceState(appManager.app.AppName, appManager.IsServiceDeleted)
	require.NoError(t, err)
	assert.Equal(t, new(apiv1.Service), svcObj)
}

func TestDeleteDeployment(t *testing.T) {
	testApp := testAppDescription()

	testSets := []struct {
		tc         string
		actionFunc func(action core.Action) (bool, runtime.Object, error)
	}{
		{
			"deployment object exists",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)
				obj := &appsv1.Deployment{}
				return true, obj, nil
			},
		},
		{
			"deployment object exists",
			func(action core.Action) (bool, runtime.Object, error) {
				err := errors.NewNotFound(
					schema.GroupResource{
						Group:    "fakeGroup",
						Resource: "fakeResource",
					},
					"deployments")

				return true, nil, err
			},
		},
	}

	for _, tt := range testSets {
		t.Run(tt.tc, func(t *testing.T) {
			client := newFakeKubeClient()
			// Set up reactor to fake verb
			client.ClientSet.(*fake.Clientset).AddReactor("delete", "deployments", tt.actionFunc)
			appManager := NewAppManager(client, testNamespace, testApp)
			err := appManager.DeleteDeployment(false)
			require.NoError(t, err)
		})
	}
}

func TestDeleteService(t *testing.T) {
	testApp := testAppDescription()

	testSets := []struct {
		tc         string
		actionFunc func(action core.Action) (bool, runtime.Object, error)
	}{
		{
			"Service object exists",
			func(action core.Action) (bool, runtime.Object, error) {
				ns := action.GetNamespace()
				assert.Equal(t, testNamespace, ns)
				obj := &apiv1.Service{}
				return true, obj, nil
			},
		},
		{
			"Service object does not exist",
			func(action core.Action) (bool, runtime.Object, error) {
				err := errors.NewNotFound(
					schema.GroupResource{
						Group:    "fakeGroup",
						Resource: "fakeResource",
					},
					"service")

				return true, nil, err
			},
		},
	}

	for _, tt := range testSets {
		t.Run(tt.tc, func(t *testing.T) {
			client := newFakeKubeClient()
			// Set up reactor to fake verb
			client.ClientSet.(*fake.Clientset).AddReactor("delete", "services", tt.actionFunc)
			appManager := NewAppManager(client, testNamespace, testApp)
			err := appManager.DeleteService(false)

			require.NoError(t, err)
		})
	}
}
