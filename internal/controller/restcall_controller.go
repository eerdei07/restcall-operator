/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	callv1alpha1 "github.com/eerdei07/restcall-operator/api/v1alpha1"
	"github.com/go-logr/logr"
)

const restcallFinalizer = "call.restcall/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableRestcall represents the status of the Deployment reconciliation
	typeAvailableRestcall = "Available"
	// typeDegradedRestcall represents the status used when the custom resource is deleted and the finalizer operations are yet to occur.
	typeDegradedRestcall = "Degraded"
)

// RestcallReconciler reconciles a Restcall object
type RestcallReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=call.restcall,resources=restcalls,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=call.restcall,resources=restcalls/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=call.restcall,resources=restcalls/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Restcall object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/reconcile
func (r *RestcallReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Restcall instance
	// The purpose is check if the Custom Resource for the Kind Restcall
	// is applied on the cluster if not we return nil to stop the reconciliation
	restcall := &callv1alpha1.Restcall{}
	err := r.Get(ctx, req.NamespacedName, restcall)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("restcall resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get restcall")
		return ctrl.Result{}, err
	}

	// Let's just set the status as Unknown when no status is available
	if restcall.Status.Conditions == nil || len(restcall.Status.Conditions) == 0 {
		meta.SetStatusCondition(&restcall.Status.Conditions, metav1.Condition{Type: typeAvailableRestcall, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, restcall); err != nil {
			log.Error(err, "Failed to update Restcall status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the restcall Custom Resource after updating the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raising the error "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
			log.Error(err, "Failed to re-fetch restcall")
			return ctrl.Result{}, err
		}
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occur before the custom resource is deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(restcall, restcallFinalizer) {
		log.Info("Adding Finalizer for Restcall")
		if ok := controllerutil.AddFinalizer(restcall, restcallFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			return ctrl.Result{Requeue: true}, nil
		}

		if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
			log.Error(err, "Failed to re-fetch restcall")
			return ctrl.Result{}, err
		}

		if err = r.Update(ctx, restcall); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}

		if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
			log.Error(err, "Failed to re-fetch restcall")
			return ctrl.Result{}, err
		}
	}

	// Check if the Restcall instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isRestcallMarkedToBeDeleted := restcall.GetDeletionTimestamp() != nil
	if isRestcallMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(restcall, restcallFinalizer) {
			log.Info("Performing Finalizer Operations for Restcall before delete CR")

			// Let's add here a status "Downgrade" to reflect that this resource began its process to be terminated.
			meta.SetStatusCondition(&restcall.Status.Conditions, metav1.Condition{Type: typeDegradedRestcall,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", restcall.Name)})

			if err := r.Status().Update(ctx, restcall); err != nil {
				log.Error(err, "Failed to update Memcached status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before removing the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForRestcall(restcall)

			// TODO(user): If you add operations to the doFinalizerOperationsForMemcached method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the memcached Custom Resource before updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
				log.Error(err, "Failed to re-fetch memcached")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&restcall.Status.Conditions, metav1.Condition{Type: typeDegradedRestcall,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", restcall.Name)})

			if err := r.Status().Update(ctx, restcall); err != nil {
				log.Error(err, "Failed to update Restcall status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Restcall after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(restcall, restcallFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Restcall")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, restcall); err != nil {
				log.Error(err, "Failed to remove finalizer for Restcall")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Check if the deployment already exists, if not create a new one
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: restcall.Name, Namespace: restcall.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new deployment
		dep, err := r.deploymentForRestcall(restcall)
		if err != nil {
			log.Error(err, "Failed to define new Deployment resource for Restcall")

			// The following implementation will update the status
			meta.SetStatusCondition(&restcall.Status.Conditions, metav1.Condition{Type: typeAvailableRestcall,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Deployment for the custom resource (%s): (%s)", restcall.Name, err)})

			if err := r.Status().Update(ctx, restcall); err != nil {
				log.Error(err, "Failed to update Restcall status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Deployment",
			"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		if err = r.Create(ctx, dep); err != nil {
			log.Error(err, "Failed to create new Deployment",
				"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}

		if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
			log.Error(err, "Failed to re-fetch restcall")
			return ctrl.Result{}, err
		}

		// Deployment created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// Make the REST call
	err = makeRestCall(log, restcall.Spec.ServiceUrl, restcall.Spec.Endpoint)
	if err != nil {
		log.Error(err, "Failed to make a rest call")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// The CRD API defines that the Restcall type have a Restcall.Endpoint field
	// to set the quantity of Deployment instances to the desired state on the cluster.
	// Therefore, the following code will ensure the Deployment Endpoint is the same as defined
	// via the Endpoint spec of the Custom Resource which we are reconciling.
	endpoint := restcall.Spec.Endpoint
	if found.Spec.Template.Spec.Containers[0].Env[0].Value != endpoint {
		found.Spec.Template.Spec.Containers[0].Env[0].Value = endpoint
		if err = r.Update(ctx, found); err != nil {
			log.Error(err, "Failed to update Deployment",
				"Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)

			// Re-fetch the rest Custom Resource before updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
				log.Error(err, "Failed to re-fetch restcall")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&restcall.Status.Conditions, metav1.Condition{Type: typeAvailableRestcall,
				Status: metav1.ConditionFalse, Reason: "EndpointChange",
				Message: fmt.Sprintf("Failed to update the size for the custom resource (%s): (%s)", restcall.Name, err)})

			if err := r.Status().Update(ctx, restcall); err != nil {
				log.Error(err, "Failed to update Restcall status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	serviceurl := restcall.Spec.ServiceUrl
	if found.Spec.Template.Spec.Containers[0].Env[1].Value != serviceurl {
		found.Spec.Template.Spec.Containers[0].Env[1].Value = serviceurl
		if err = r.Update(ctx, found); err != nil {
			log.Error(err, "Failed to update Deployment",
				"Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)

			// Re-fetch the rest Custom Resource before updating the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raising the error "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
				log.Error(err, "Failed to re-fetch rest")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&restcall.Status.Conditions, metav1.Condition{Type: typeAvailableRestcall,
				Status: metav1.ConditionFalse, Reason: "ServiceChange",
				Message: fmt.Sprintf("Failed to update the size for the custom resource (%s): (%s)", restcall.Name, err)})

			if err := r.Status().Update(ctx, restcall); err != nil {
				log.Error(err, "Failed to update Restcall status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	// The following implementation will update the status
	meta.SetStatusCondition(&restcall.Status.Conditions, metav1.Condition{Type: typeAvailableRestcall,
		Status: metav1.ConditionTrue, Reason: "Reconciling",
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %s endpoint created successfully", restcall.Name, restcall.Spec.Endpoint)})

	if err := r.Status().Update(ctx, restcall); err != nil {
		log.Error(err, "Failed to update Restcall status")
		return ctrl.Result{}, err
	}

	if err := r.Get(ctx, req.NamespacedName, restcall); err != nil {
		log.Error(err, "Failed to re-fetch restcall")
		return ctrl.Result{}, err
	}

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

func makeRestCall(log logr.Logger, serviceUrl string, endpoint string) error {
	log.Info("REST call GET: ", "host: ", serviceUrl, "endpoint: ", endpoint)
	resp, error := http.Get(serviceUrl + endpoint)
	if error != nil {
		// we will get an error at this stage if the request fails, such as if the
		// requested URL is not found, or if the server is not reachable.
		log.Error(error, "Failed to get: ", "host: ", serviceUrl, "endponit: ", endpoint)
		return error
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// if the status code is not 200, we should log the status code and the
		// status string, then exit with a fatal error
		log.Error(error, "status code error: ", strconv.Itoa(resp.StatusCode), "status: ", resp.Status)
		return errors.NewGone("Status is not OK")
	}
	data, err := io.ReadAll(resp.Body)
	log.Info("Result: ", "response body: ", string(data))
	return err
}

// finalizeRestcall will perform the required operations before delete the CR.
func (r *RestcallReconciler) doFinalizerOperationsForRestcall(cr *callv1alpha1.Restcall) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of deleting resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as dependent of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

// deploymentForRestcall returns a Restcall Deployment object
func (r *RestcallReconciler) deploymentForRestcall(
	restcall *callv1alpha1.Restcall) (*appsv1.Deployment, error) {
	ls := labelsForRestcall(restcall.Name)
	// replicas := rest.Spec.Size

	// Get the Operand image
	image, err := imageForRestcall()
	if err != nil {
		return nil, err
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restcall.Name,
			Namespace: restcall.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					// TODO(user): Uncomment the following code to configure the nodeAffinity expression
					// according to the platforms which are supported by your solution. It is considered
					// best practice to support multiple architectures. build your manager image using the
					// makefile target docker-buildx. Also, you can use docker manifest inspect <image>
					// to check what are the platforms supported.
					// More info: https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-affinity
					//Affinity: &corev1.Affinity{
					//	NodeAffinity: &corev1.NodeAffinity{
					//		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					//			NodeSelectorTerms: []corev1.NodeSelectorTerm{
					//				{
					//					MatchExpressions: []corev1.NodeSelectorRequirement{
					//						{
					//							Key:      "kubernetes.io/arch",
					//							Operator: "In",
					//							Values:   []string{"amd64", "arm64", "ppc64le", "s390x"},
					//						},
					//						{
					//							Key:      "kubernetes.io/os",
					//							Operator: "In",
					//							Values:   []string{"linux"},
					//						},
					//					},
					//				},
					//			},
					//		},
					//	},
					//},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &[]bool{true}[0],
						// IMPORTANT: seccomProfile was introduced with Kubernetes 1.19
						// If you are looking for to produce solutions to be supported
						// on lower versions you must remove this option.
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Image: image,
						Name:  "rest",
						Env: []corev1.EnvVar{
							{
								Name:  "REST_URL",
								Value: restcall.Spec.Endpoint,
							},
							{
								Name:  "SERVICE",
								Value: restcall.Spec.ServiceUrl,
							}},
						ImagePullPolicy: corev1.PullIfNotPresent,
						// Ensure restrictive context for the container
						// More info: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
						SecurityContext: &corev1.SecurityContext{
							// WARNING: Ensure that the image used defines an UserID in the Dockerfile
							// otherwise the Pod will not run and will fail with "container has runAsNonRoot and image has non-numeric user"".
							// If you want your workloads admitted in namespaces enforced with the restricted mode in OpenShift/OKD vendors
							// then, you MUST ensure that the Dockerfile defines a User ID OR you MUST leave the "RunAsNonRoot" and
							// "RunAsUser" fields empty.
							RunAsNonRoot: &[]bool{true}[0],
							// The rest image does not use a non-zero numeric user as the default user.
							// Due to RunAsNonRoot field being set to true, we need to force the user in the
							// container to a non-zero numeric user. We do this using the RunAsUser field.
							// However, if you are looking to provide solution for K8s vendors like OpenShift
							// be aware that you cannot run under its restricted-v2 SCC if you set this value.
							RunAsUser:                &[]int64{1001}[0],
							AllowPrivilegeEscalation: &[]bool{false}[0],
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{
									"ALL",
								},
							},
						},
						Ports: []corev1.ContainerPort{{
							ContainerPort: 9001,
							Name:          "restcall",
						}},
						// Command: []string{"rest", "-m=64", "-o", "modern", "-v"},
						// Command: []string{"/bin/sh", "-c"},
						// Args:    []string{"apt-get update && apt-get install curl && " + createCurlCommand(rest)},
					}},
				},
			},
		},
	}

	// Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(restcall, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// labelsForRestcall returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForRestcall(name string) map[string]string {
	var imageTag string
	image, err := imageForRestcall()
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{"app.kubernetes.io/name": "restcall-operator",
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/managed-by": "RestcallController",
	}
}

// imageForRestcall gets the Operand image which is managed by this controller
// from the RESTCALL_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForRestcall() (string, error) {
	var imageEnvVar = "RESTCALL_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find %s environment variable with the image", imageEnvVar)
	}
	return image, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RestcallReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&callv1alpha1.Restcall{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
