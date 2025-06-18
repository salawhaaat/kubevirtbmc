package bmc

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	bmcsv1beta1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type VirtualMachineBMCReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	AgentImageName string
	AgentImageTag  string
}

func (r *VirtualMachineBMCReconciler) setCondition(ctx context.Context, vmBMC *bmcsv1beta1.VirtualMachineBMC, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: vmBMC.Generation,
	}
	conditions := vmBMC.Status.Conditions
	found := false
	for i, cond := range conditions {
		if cond.Type == conditionType {
			if cond.Status != status || cond.Reason != reason || cond.Message != message {
				conditions[i] = condition
			}
			found = true
			break
		}
	}
	if !found {
		conditions = append(conditions, condition)
	}
	vmBMC.Status.Conditions = conditions
}

func (r *VirtualMachineBMCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("virtualmachinebmc", req.NamespacedName)
	virtualMachineBMC, result, err := r.fetchVirtualMachineBMC(ctx, req, logger)
	if result != nil || err != nil {
		return *result, err
	}
	result, err = r.handleFinalizer(ctx, virtualMachineBMC, logger)
	if result != nil || err != nil {
		return *result, err
	}
	vm, secret, result, err := r.validateReferences(ctx, virtualMachineBMC, logger)
	if result != nil || err != nil {
		return *result, err
	}
	foundService, foundDeployment, result, err := r.reconcileResources(ctx, virtualMachineBMC, vm, secret, logger)
	if result != nil || err != nil {
		return *result, err
	}
	result, err = r.updateStatus(ctx, virtualMachineBMC, foundService, foundDeployment, logger)
	if result != nil || err != nil {
		return *result, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *VirtualMachineBMCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bmcsv1beta1.VirtualMachineBMC{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func (r *VirtualMachineBMCReconciler) fetchVirtualMachineBMC(ctx context.Context, req ctrl.Request, logger logr.Logger) (*bmcsv1beta1.VirtualMachineBMC, *ctrl.Result, error) {
	virtualMachineBMC := &bmcsv1beta1.VirtualMachineBMC{}
	if err := r.Get(ctx, req.NamespacedName, virtualMachineBMC); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("VirtualMachineBMC resource not found. Ignoring since object must be deleted.")
			return nil, &ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get VirtualMachineBMC")
		return nil, &ctrl.Result{}, err
	}
	return virtualMachineBMC, nil, nil
}

func (r *VirtualMachineBMCReconciler) handleFinalizer(ctx context.Context, vmBMC *bmcsv1beta1.VirtualMachineBMC, logger logr.Logger) (*ctrl.Result, error) {
	finalizer := "bmc.kubevirt.io/finalizer"
	if vmBMC.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(vmBMC, finalizer) {
			logger.Info("Adding finalizer to VirtualMachineBMC")
			controllerutil.AddFinalizer(vmBMC, finalizer)
			if err := r.Update(ctx, vmBMC); err != nil {
				logger.Error(err, "Failed to add finalizer to VirtualMachineBMC")
				return &ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(vmBMC, finalizer) {
			if err := r.cleanupOwnedResources(ctx, vmBMC, logger); err != nil {
				return &ctrl.Result{}, err
			}
			logger.Info("Removing finalizer from VirtualMachineBMC")
			controllerutil.RemoveFinalizer(vmBMC, finalizer)
			if err := r.Update(ctx, vmBMC); err != nil {
				logger.Error(err, "Failed to remove finalizer from VirtualMachineBMC")
				return &ctrl.Result{}, err
			}
		}
		return &ctrl.Result{}, nil
	}
	return nil, nil
}

func (r *VirtualMachineBMCReconciler) cleanupOwnedResources(ctx context.Context, vmBMC *bmcsv1beta1.VirtualMachineBMC, logger logr.Logger) error {
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: vmBMC.Name + "-bmc-proxy", Namespace: vmBMC.Namespace}, deployment)
	if err == nil {
		logger.Info("Deleting associated BMC proxy Deployment")
		if err := r.Delete(ctx, deployment); client.IgnoreNotFound(err) != nil {
			logger.Error(err, "Failed to delete BMC proxy Deployment")
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to get BMC proxy Deployment for deletion check")
		return err
	}
	service := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: vmBMC.Name + "-bmc-service", Namespace: vmBMC.Namespace}, service)
	if err == nil {
		logger.Info("Deleting associated BMC Service")
		if err := r.Delete(ctx, service); client.IgnoreNotFound(err) != nil {
			logger.Error(err, "Failed to delete BMC Service")
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to get BMC Service for deletion check")
		return err
	}
	return nil
}

func (r *VirtualMachineBMCReconciler) validateReferences(ctx context.Context, vmBMC *bmcsv1beta1.VirtualMachineBMC, logger logr.Logger) (*kubevirtv1.VirtualMachine, *corev1.Secret, *ctrl.Result, error) {
	vm := &kubevirtv1.VirtualMachine{}
	if vmBMC.Spec.VirtualMachineRef == nil || vmBMC.Spec.VirtualMachineRef.Name == "" {
		r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "VirtualMachineRefMissing", "VirtualMachineRef is required and must specify a name.")
		return nil, nil, &ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	err := r.Get(ctx, types.NamespacedName{Name: vmBMC.Spec.VirtualMachineRef.Name, Namespace: vmBMC.Namespace}, vm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Referenced VirtualMachine not found", "VMName", vmBMC.Spec.VirtualMachineRef.Name)
			r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "VirtualMachineNotFound", fmt.Sprintf("Referenced VirtualMachine '%s' not found.", vmBMC.Spec.VirtualMachineRef.Name))
			return nil, nil, &ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		logger.Error(err, "Failed to get referenced VirtualMachine")
		return nil, nil, &ctrl.Result{}, err
	}
	secret := &corev1.Secret{}
	if vmBMC.Spec.AuthSecretRef == nil || vmBMC.Spec.AuthSecretRef.Name == "" {
		r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "AuthSecretRefMissing", "AuthSecretRef is required and must specify a name.")
		return nil, nil, &ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	err = r.Get(ctx, types.NamespacedName{Name: vmBMC.Spec.AuthSecretRef.Name, Namespace: vmBMC.Namespace}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Referenced AuthSecret not found", "SecretName", vmBMC.Spec.AuthSecretRef.Name)
			r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "AuthSecretNotFound", fmt.Sprintf("Referenced AuthSecret '%s' not found.", vmBMC.Spec.AuthSecretRef.Name))
			return nil, nil, &ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		logger.Error(err, "Failed to get referenced AuthSecret")
		return nil, nil, &ctrl.Result{}, err
	}
	r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionUnknown, "ReferencesValidated", "All required references (VirtualMachine, AuthSecret) are found and valid.")
	return vm, secret, nil, nil
}

func (r *VirtualMachineBMCReconciler) reconcileResources(ctx context.Context, vmBMC *bmcsv1beta1.VirtualMachineBMC, vm *kubevirtv1.VirtualMachine, secret *corev1.Secret, logger logr.Logger) (*corev1.Service, *appsv1.Deployment, *ctrl.Result, error) {
	desiredService := r.serviceForVirtualMachineBMC(vmBMC)
	foundService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desiredService.Name, Namespace: desiredService.Namespace}, foundService)
	if err != nil && apierrors.IsNotFound(err) {
		logger.Info("Creating a new BMC Service", "Service.Namespace", desiredService.Namespace, "Service.Name", desiredService.Name)
		if err = r.Create(ctx, desiredService); err != nil {
			logger.Error(err, "Failed to create new BMC Service")
			r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "ServiceCreationFailed", fmt.Sprintf("Failed to create Service: %s", err.Error()))
			return nil, nil, &ctrl.Result{}, err
		}
		foundService = desiredService
	} else if err != nil {
		logger.Error(err, "Failed to get existing BMC Service")
		r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "ServiceFetchFailed", fmt.Sprintf("Failed to get Service: %s", err.Error()))
		return nil, nil, &ctrl.Result{}, err
	} else {
		updatedService := foundService.DeepCopy()
		updatedService.Spec.Selector = desiredService.Spec.Selector
		updatedService.Spec.Ports = desiredService.Spec.Ports
		if !reflect.DeepEqual(foundService.Spec, updatedService.Spec) {
			logger.Info("Updating existing BMC Service", "Service.Namespace", updatedService.Namespace, "Service.Name", updatedService.Name)
			if err := r.Update(ctx, updatedService); err != nil {
				logger.Error(err, "Failed to update existing BMC Service")
				r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "ServiceUpdateFailed", fmt.Sprintf("Failed to update Service: %s", err.Error()))
				return nil, nil, &ctrl.Result{}, err
			}
			foundService = updatedService
		}
	}
	desiredDeployment := r.deploymentForVirtualMachineBMC(vmBMC, vm, secret)
	foundDeployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: desiredDeployment.Name, Namespace: desiredDeployment.Namespace}, foundDeployment)
	if err != nil && apierrors.IsNotFound(err) {
		logger.Info("Creating a new BMC Proxy Deployment", "Deployment.Namespace", desiredDeployment.Namespace, "Deployment.Name", desiredDeployment.Name)
		if err = r.Create(ctx, desiredDeployment); err != nil {
			logger.Error(err, "Failed to create new BMC Proxy Deployment")
			r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "DeploymentCreationFailed", fmt.Sprintf("Failed to create Deployment: %s", err.Error()))
			return nil, nil, &ctrl.Result{}, err
		}
		foundDeployment = desiredDeployment
	} else if err != nil {
		logger.Error(err, "Failed to get existing BMC Proxy Deployment")
		r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "DeploymentFetchFailed", fmt.Sprintf("Failed to get Deployment: %s", err.Error()))
		return nil, nil, &ctrl.Result{}, err
	} else {
		updatedDeployment := foundDeployment.DeepCopy()
		if !reflect.DeepEqual(foundDeployment.Spec.Replicas, desiredDeployment.Spec.Replicas) ||
			!reflect.DeepEqual(foundDeployment.Spec.Template.Spec.Containers[0].Image, desiredDeployment.Spec.Template.Spec.Containers[0].Image) ||
			!reflect.DeepEqual(foundDeployment.Spec.Template.Spec.Volumes, desiredDeployment.Spec.Template.Spec.Volumes) {
			updatedDeployment.Spec.Replicas = desiredDeployment.Spec.Replicas
			updatedDeployment.Spec.Template = desiredDeployment.Spec.Template
			logger.Info("Updating existing BMC Proxy Deployment", "Deployment.Namespace", updatedDeployment.Namespace, "Deployment.Name", updatedDeployment.Name)
			if err := r.Update(ctx, updatedDeployment); err != nil {
				logger.Error(err, "Failed to update existing BMC Proxy Deployment")
				r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "DeploymentUpdateFailed", fmt.Sprintf("Failed to update Deployment: %s", err.Error()))
				return nil, nil, &ctrl.Result{}, err
			}
			foundDeployment = updatedDeployment
		}
	}
	return foundService, foundDeployment, nil, nil
}

func (r *VirtualMachineBMCReconciler) updateStatus(ctx context.Context, vmBMC *bmcsv1beta1.VirtualMachineBMC, foundService *corev1.Service, foundDeployment *appsv1.Deployment, logger logr.Logger) (*ctrl.Result, error) {
	if foundService.Spec.ClusterIP != "" && vmBMC.Status.ClusterIP != foundService.Spec.ClusterIP {
		vmBMC.Status.ClusterIP = foundService.Spec.ClusterIP
		r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionTrue, "ClusterIPUpdated", "VirtualMachineBMC status ClusterIP updated.")
		logger.Info("Updating VirtualMachineBMC status with ClusterIP", "ClusterIP", foundService.Spec.ClusterIP)
		if err := r.Status().Update(ctx, vmBMC); err != nil {
			logger.Error(err, "Failed to update VirtualMachineBMC status ClusterIP")
			return &ctrl.Result{}, err
		}
	}
	if foundDeployment.Status.AvailableReplicas > 0 && foundDeployment.Status.ReadyReplicas == *foundDeployment.Spec.Replicas {
		r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionTrue, "BMCServiceReady", "VirtualMachineBMC service is ready.")
	} else {
		r.setCondition(ctx, vmBMC, bmcsv1beta1.ConditionReady, metav1.ConditionFalse, "BMCServiceNotReady", "VirtualMachineBMC service is not yet ready.")
	}
	if err := r.Status().Update(ctx, vmBMC); err != nil {
		logger.Error(err, "Failed to update VirtualMachineBMC status conditions")
		return &ctrl.Result{}, err
	}
	return nil, nil
}

func (r *VirtualMachineBMCReconciler) serviceForVirtualMachineBMC(vmBMC *bmcsv1beta1.VirtualMachineBMC) *corev1.Service {
	labels := map[string]string{
		"app": vmBMC.Name + "-bmc-proxy",
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmBMC.Name + "-bmc-service",
			Namespace: vmBMC.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vmBMC, bmcsv1beta1.GroupVersion.WithKind("VirtualMachineBMC")),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:     "bmc",
					Port:     623,
					Protocol: corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

func (r *VirtualMachineBMCReconciler) deploymentForVirtualMachineBMC(vmBMC *bmcsv1beta1.VirtualMachineBMC, vm *kubevirtv1.VirtualMachine, secret *corev1.Secret) *appsv1.Deployment {
	labels := map[string]string{
		"app": vmBMC.Name + "-bmc-proxy",
	}
	replicas := int32(1)
	image := r.AgentImageName
	if r.AgentImageTag != "" {
		image = fmt.Sprintf("%s:%s", r.AgentImageName, r.AgentImageTag)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmBMC.Name + "-bmc-proxy",
			Namespace: vmBMC.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vmBMC, bmcsv1beta1.GroupVersion.WithKind("VirtualMachineBMC")),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "bmc-proxy",
							Image: image,
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 623,
									Name:          "bmc",
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "VIRTUALMACHINE_NAME",
									Value: vm.Name,
								},
								{
									Name:  "VIRTUALMACHINE_NAMESPACE",
									Value: vm.Namespace,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "auth-secret",
									MountPath: "/etc/bmc-auth",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "auth-secret",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secret.Name,
								},
							},
						},
					},
				},
			},
		},
	}
}
