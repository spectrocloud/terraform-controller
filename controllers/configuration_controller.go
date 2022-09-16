/*
Copyright 2021 The KubeVela Authors.

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
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/acarl005/stripansi"
	"github.com/oam-dev/terraform-controller/api/types"
	crossplane "github.com/oam-dev/terraform-controller/api/types/crossplane-runtime"
	"github.com/oam-dev/terraform-controller/api/v1beta1"
	tfcfg "github.com/oam-dev/terraform-controller/controllers/configuration"
	"github.com/oam-dev/terraform-controller/controllers/provider"
	"github.com/oam-dev/terraform-controller/controllers/terraform"
	"github.com/oam-dev/terraform-controller/controllers/util"
)

const (
	terraformWorkspace = "default"
	// WorkingVolumeMountPath is the mount path for working volume
	WorkingVolumeMountPath = "/data"
	// InputTFConfigurationVolumeName is the volume name for input Terraform Configuration
	InputTFConfigurationVolumeName = "tf-input-configuration"
	// BackendVolumeName is the volume name for Terraform backend
	BackendVolumeName = "tf-backend"
	// InputTFConfigurationVolumeMountPath is the volume mount path for input Terraform Configuration
	InputTFConfigurationVolumeMountPath = "/opt/tf-configuration"
	// BackendVolumeMountPath is the volume mount path for Terraform backend
	BackendVolumeMountPath = "/opt/tf-backend"
)

const (
	// TerraformStateNameInSecret is the key name to store Terraform state
	TerraformStateNameInSecret = "tfstate"
	// TFInputConfigMapName is the CM name for Terraform Input Configuration
	TFInputConfigMapName = "tf-%s"
	// TFVariableSecret is the Secret name for variables, including credentials from Provider
	TFVariableSecret = "variable-%s"
	// TFBackendSecret is the Secret name for Kubernetes backend
	TFBackendSecret = "tfstate-%s-%s"
)

// TerraformExecutionType is the type for Terraform execution
type TerraformExecutionType string

const (
	// TerraformApply is the name to mark `terraform apply`
	TerraformApply TerraformExecutionType = "apply"
	// TerraformDestroy is the name to mark `terraform destroy`
	TerraformDestroy TerraformExecutionType = "destroy"
)

const (
	configurationFinalizer = "configuration.finalizers.terraform-controller"
	// ClusterRoleName is the name of the ClusterRole for Terraform Job
	ClusterRoleName = "tf-executor-clusterrole"
	// ServiceAccountName is the name of the ServiceAccount for Terraform Job
	ServiceAccountName = "tf-executor-service-account"
	// VariableSecretHashKey is the name of the annotation for Terraform Job variable secret to find out changes in values
	VariableSecretHashAnnotationKey = "last-applied-variables-hash"
)

// ConfigurationReconciler reconciles a Configuration object.
type ConfigurationReconciler struct {
	client.Client
	Log          logr.Logger
	Scheme       *runtime.Scheme
	ProviderName string
}

// +kubebuilder:rbac:groups=terraform.core.oam.dev,resources=configurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=terraform.core.oam.dev,resources=configurations/status,verbs=get;update;patch

// Reconcile will reconcile periodically
func (r *ConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.InfoS("reconciling Terraform Configuration...", "NamespacedName", req.NamespacedName)

	configuration, err := tfcfg.Get(ctx, r.Client, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	annotations := configuration.GetAnnotations()
	if val, ok := annotations["vmoperator.cluster.spectrocloud.com/paused"]; ok && val == "true" {
		klog.InfoS("returning early as configuration is paused", "annotations[vmoperator.cluster.spectrocloud.com/paused]", val)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if configuration.Spec.ProviderReference != nil && configuration.Spec.ProviderReference.Namespace == provider.DefaultNamespace {
		configuration.Spec.ProviderReference.Namespace = configuration.Namespace
	}

	meta := initTFConfigurationMeta(req, configuration)

	// add finalizer
	var isDeleting = !configuration.ObjectMeta.DeletionTimestamp.IsZero()
	if !isDeleting {

		if prvdr, err := provider.GetProviderFromConfiguration(ctx, r.Client, meta.ProviderReference.Namespace, meta.ProviderReference.Name); err != nil {
			return ctrl.Result{RequeueAfter: 1 * time.Second}, errors.Wrap(err, "failed to get provider object")
		} else if prvdr != nil {
			if !controllerutil.ContainsFinalizer(prvdr, configurationFinalizer) {
				controllerutil.AddFinalizer(prvdr, configurationFinalizer)
				if err := r.Update(ctx, prvdr); err != nil {
					return ctrl.Result{RequeueAfter: 1 * time.Second}, errors.Wrap(err, "failed to add finalizer to provider object")
				}
			}
		}

		if !controllerutil.ContainsFinalizer(&configuration, configurationFinalizer) {
			controllerutil.AddFinalizer(&configuration, configurationFinalizer)
			if configuration.Spec.ProviderReference != nil && configuration.Spec.ProviderReference.Namespace == provider.DefaultNamespace {
				configuration.Spec.ProviderReference.Namespace = configuration.Namespace
			}

			if err := r.Update(ctx, &configuration); err != nil {
				return ctrl.Result{RequeueAfter: 3 * time.Second}, errors.Wrap(err, "failed to add finalizer")
			}
		}
	}

	// pre-check Configuration
	if err := r.preCheck(ctx, &configuration, meta); err != nil && !isDeleting {
		return ctrl.Result{}, err
	}

	var tfExecutionJob = &batchv1.Job{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: meta.ApplyJobName, Namespace: meta.Namespace}, tfExecutionJob); err == nil {
		if tfExecutionJob.Status.Succeeded == int32(1) {
			if err := meta.updateApplyStatus(ctx, r.Client, types.Available, types.MessageCloudResourceDeployed); err != nil {
				return ctrl.Result{}, err
			}

			//if job is 2 minutes older than delete it..
			compTime := tfExecutionJob.Status.CompletionTime
			if compTime != nil && compTime.Add(2*time.Minute).Before(time.Now()) {
				klog.Info("Deleting job %s as to reconcile it", tfExecutionJob.Name)
				if err := r.Client.Delete(ctx, tfExecutionJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else {
			// if job is 5 minutes older and has not succeeded then delete it..
			startTime := tfExecutionJob.Status.StartTime
			if startTime != nil && startTime.Add(5*time.Minute).Before(time.Now()) {
				pods := &v1.PodList{}
				r.Client.List(ctx, pods, client.MatchingLabels(map[string]string{"job-name": meta.ApplyJobName}),
					client.InNamespace(meta.Namespace),
				)
				deleteJob := false
				for _, pod := range pods.Items {
					if pod.Status.Phase != v1.PodRunning {
						deleteJob = true
						break
					}
					for _, container := range pod.Status.ContainerStatuses {
						if container.RestartCount > 5 {
							deleteJob = true
							break
						}
					}
				}
				if deleteJob {
					klog.Info("Deleting job %s as to reconcile it", tfExecutionJob.Name)
					if err := r.Client.Delete(ctx, tfExecutionJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
						return ctrl.Result{}, err
					}
				}
			}
		}
	}

	if err := r.Client.Get(ctx, client.ObjectKey{Name: meta.DestroyJobName, Namespace: meta.Namespace}, tfExecutionJob); err == nil {
		if tfExecutionJob.Status.Succeeded == int32(1) {
			if err := meta.updateApplyStatus(ctx, r.Client, types.DestroyCompleted, types.MessageDestroyCompleted); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if isDeleting {
		// terraform destroy
		klog.InfoS("performing Configuration Destroy", "Namespace", req.Namespace, "Name", req.Name, "JobName", meta.DestroyJobName)

		_, err := terraform.GetTerraformStatus(ctx, meta.Namespace, meta.DestroyJobName)
		if err != nil {
			klog.ErrorS(err, "Terraform destroy failed")
			msg := stripansi.Strip(err.Error())
			if updateErr := meta.updateDestroyStatus(ctx, r.Client, types.ConfigurationDestroyFailed, msg); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
		}

		if err := r.terraformDestroy(ctx, req.Namespace, configuration, meta); err != nil {
			if err.Error() == types.MessageDestroyJobNotCompleted {
				return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
			}
			return ctrl.Result{RequeueAfter: 3 * time.Second}, errors.Wrap(err, "continue reconciling to destroy cloud resource")
		}

		removeFinalizerFunc := func() error {
			if prvdr, err := provider.GetProviderFromConfiguration(ctx, r.Client, meta.ProviderReference.Namespace, meta.ProviderReference.Name); err != nil {
				return client.IgnoreNotFound(err)
			} else if prvdr != nil {
				if controllerutil.ContainsFinalizer(prvdr, configurationFinalizer) {
					controllerutil.RemoveFinalizer(prvdr, configurationFinalizer)
					if err := r.Update(ctx, prvdr); err != nil {
						return errors.Wrap(err, "failed to add finalizer to provider object")
					}
				}

			}
			if confi, err := tfcfg.Get(ctx, r.Client, req.NamespacedName); err != nil {
				return client.IgnoreNotFound(err)
			} else {
				if controllerutil.ContainsFinalizer(&confi, configurationFinalizer) {
					controllerutil.RemoveFinalizer(&confi, configurationFinalizer)
					if configuration.Spec.ProviderReference != nil && configuration.Spec.ProviderReference.Namespace == provider.DefaultNamespace {
						configuration.Spec.ProviderReference.Namespace = configuration.Namespace
					}
					if err := r.Update(ctx, &confi); err != nil {
						return errors.Wrap(err, "failed to remove finalizer")
					}
				}
			}
			klog.Info("Removed finalizer")
			return nil
		}

		if err := util.Retry(removeFinalizerFunc, 3*time.Second, 50); err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, errors.Wrap(err, "failed to remove finalizer")
		} else {
			return ctrl.Result{}, nil
		}
	}

	// Terraform apply (create or update)
	klog.InfoS("performing Terraform Apply (cloud resource create/update)", "Namespace", req.Namespace, "Name", req.Name)
	if err := r.terraformApply(ctx, req.Namespace, configuration, meta); err != nil {
		if err.Error() == types.MessageApplyJobNotCompleted {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, errors.Wrap(err, "failed to create/update cloud resource")
	}
	state, err := terraform.GetTerraformStatus(ctx, meta.Namespace, meta.ApplyJobName)
	if err != nil {
		klog.Error(stripansi.Strip(err.Error()), "Terraform apply failed")
		if updateErr := meta.updateApplyStatus(ctx, r.Client, state, err.Error()); updateErr != nil {
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, updateErr
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	return ctrl.Result{}, nil
}

// TFConfigurationMeta is all the metadata of a Configuration
type TFConfigurationMeta struct {
	Name                  string
	Namespace             string
	ConfigurationType     types.ConfigurationType
	CompleteConfiguration string
	RemoteGit             string
	RemoteGitPath         string
	ConfigurationChanged  bool
	ConfigurationCMName   string
	BackendSecretName     string
	ApplyJobName          string
	DestroyJobName        string
	Envs                  []v1.EnvVar
	ProviderReference     *crossplane.Reference
	VariableSecretName    string
	VariableSecretData    map[string][]byte
	DeleteResource        bool
	Credentials           map[string]string

	// TerraformImage is the Terraform image which can run `terraform init/plan/apply`
	TerraformImage            string
	TerraformBackendNamespace string
	BusyboxImage              string
	GitImage                  string
	DeleteSecretOnJobUpdate   bool
}

func initTFConfigurationMeta(req ctrl.Request, configuration v1beta1.Configuration) *TFConfigurationMeta {
	var meta = &TFConfigurationMeta{
		Namespace:               req.Namespace,
		Name:                    req.Name,
		ConfigurationCMName:     fmt.Sprintf(TFInputConfigMapName, req.Name),
		VariableSecretName:      fmt.Sprintf(TFVariableSecret, req.Name),
		ApplyJobName:            req.Name + "-" + string(TerraformApply),
		DestroyJobName:          req.Name + "-" + string(TerraformDestroy),
		DeleteSecretOnJobUpdate: true,
	}

	// githubBlocked mark whether GitHub is blocked in the cluster
	githubBlockedStr := os.Getenv("GITHUB_BLOCKED")
	if githubBlockedStr == "" {
		githubBlockedStr = "false"
	}

	meta.RemoteGit = tfcfg.ReplaceTerraformSource(configuration.Spec.Remote, githubBlockedStr)
	meta.DeleteResource = configuration.Spec.DeleteResource
	if configuration.Spec.Path == "" {
		meta.RemoteGitPath = "."
	} else {
		meta.RemoteGitPath = configuration.Spec.Path
	}

	meta.ProviderReference = tfcfg.GetProviderNamespacedName(configuration)

	// Check the existence of Terraform state secret which is used to store TF state file. For detailed information,
	// please refer to https://www.terraform.io/docs/language/settings/backends/kubernetes.html#configuration-variables
	var backendSecretSuffix string
	if configuration.Spec.Backend != nil && configuration.Spec.Backend.SecretSuffix != "" {
		backendSecretSuffix = configuration.Spec.Backend.SecretSuffix
	} else {
		backendSecretSuffix = configuration.Name
	}
	// Secrets will be named in the format: tfstate-{workspace}-{secret_suffix}
	meta.BackendSecretName = fmt.Sprintf(TFBackendSecret, terraformWorkspace, backendSecretSuffix)

	return meta
}

func (r *ConfigurationReconciler) terraformApply(ctx context.Context, namespace string, configuration v1beta1.Configuration, meta *TFConfigurationMeta) error {
	klog.InfoS("terraform apply job", "Namespace", namespace, "Name", meta.ApplyJobName)

	var (
		k8sClient      = r.Client
		tfExecutionJob batchv1.Job
	)

	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.ApplyJobName, Namespace: namespace}, &tfExecutionJob); err != nil {
		if kerrors.IsNotFound(err) {
			return meta.assembleAndTriggerJob(ctx, k8sClient, &configuration, TerraformApply)
		}
	}

	if err := meta.updateTerraformJobIfNeeded(ctx, k8sClient, configuration, tfExecutionJob); err != nil {
		klog.ErrorS(err, types.ErrUpdateTerraformApplyJob, "Name", meta.ApplyJobName)
		return errors.Wrap(err, types.ErrUpdateTerraformApplyJob)
	}

	if tfExecutionJob.Status.Succeeded == int32(1) {
		if err := meta.updateApplyStatus(ctx, k8sClient, types.Available, types.MessageCloudResourceDeployed); err != nil {
			return err
		}
	} else {
		// start provisioning and check the status of the provision
		// If the state is types.InvalidRegion, no need to continue checking
		if configuration.Status.Apply.State != types.ConfigurationProvisioningAndChecking &&
			configuration.Status.Apply.State != types.InvalidRegion {
			if err := meta.updateApplyStatus(ctx, r.Client, types.ConfigurationProvisioningAndChecking, types.MessageCloudResourceProvisioningAndChecking); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ConfigurationReconciler) terraformDestroy(ctx context.Context, namespace string, configuration v1beta1.Configuration, meta *TFConfigurationMeta) error {
	var (
		destroyJob batchv1.Job
		k8sClient  = r.Client
	)

	deletable, err := tfcfg.IsDeletable(ctx, k8sClient, &configuration)
	if err != nil {
		return err
	}

	deleteConfigurationDirectly := deletable || !meta.DeleteResource

	if !deleteConfigurationDirectly {
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.DestroyJobName, Namespace: meta.Namespace}, &destroyJob); err != nil {
			if kerrors.IsNotFound(err) {
				if err := r.Client.Get(ctx, client.ObjectKey{Name: configuration.Name, Namespace: configuration.Namespace}, &v1beta1.Configuration{}); err == nil {
					if err = meta.assembleAndTriggerJob(ctx, k8sClient, &configuration, TerraformDestroy); err != nil {
						return err
					}
				}
			}
		}
		if err := meta.updateTerraformJobIfNeeded(ctx, k8sClient, configuration, destroyJob); err != nil {
			klog.ErrorS(err, types.ErrUpdateTerraformApplyJob, "Name", meta.ApplyJobName)
			return errors.Wrap(err, types.ErrUpdateTerraformApplyJob)
		}
	}

	// destroying
	if err := meta.updateDestroyStatus(ctx, k8sClient, types.ConfigurationDestroying, types.MessageCloudResourceDestroying); err != nil {
		return err
	}

	// When the deletion Job process succeeded, clean up work is starting.
	if destroyJob.Status.Succeeded == int32(1) || deleteConfigurationDirectly {
		// 1. delete Terraform input Configuration ConfigMap
		if err := meta.deleteConfigMap(ctx, k8sClient); err != nil {
			return err
		}

		// 2. delete connectionSecret
		if configuration.Spec.WriteConnectionSecretToReference != nil {
			secretName := configuration.Spec.WriteConnectionSecretToReference.Name
			secretNameSpace := configuration.Spec.WriteConnectionSecretToReference.Namespace
			if len(secretNameSpace) == 0 {
				secretNameSpace = configuration.Namespace
			}
			if err := deleteConnectionSecret(ctx, k8sClient, secretName, secretNameSpace); err != nil {
				return err
			}
		}

		// 3. delete apply job
		var applyJob batchv1.Job
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.ApplyJobName, Namespace: namespace}, &applyJob); err == nil {
			if err := k8sClient.Delete(ctx, &applyJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return err
			}
		}

		// 4. delete destroy job
		var j batchv1.Job
		if err := r.Client.Get(ctx, client.ObjectKey{Name: destroyJob.Name, Namespace: destroyJob.Namespace}, &j); err == nil {
			if err := r.Client.Delete(ctx, &j, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return err
			}
		}

		// 5. delete secret which stores variables
		klog.InfoS("Deleting the secret which stores variables", "Name", meta.VariableSecretName)
		var variableSecret v1.Secret
		if err := r.Client.Get(ctx, client.ObjectKey{Name: meta.VariableSecretName, Namespace: meta.Namespace}, &variableSecret); err == nil {
			if err := r.Client.Delete(ctx, &variableSecret); err != nil {
				return err
			}
		}

		// 6. delete Kubernetes backend secret
		klog.InfoS("Deleting the secret which stores Kubernetes backend", "Name", meta.BackendSecretName)
		var kubernetesBackendSecret v1.Secret
		if err := r.Client.Get(ctx, client.ObjectKey{Name: meta.BackendSecretName, Namespace: meta.TerraformBackendNamespace}, &kubernetesBackendSecret); err == nil {
			if err := r.Client.Delete(ctx, &kubernetesBackendSecret); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New(types.MessageDestroyJobNotCompleted)
}

func (r *ConfigurationReconciler) preCheck(ctx context.Context, configuration *v1beta1.Configuration, meta *TFConfigurationMeta) error {
	var k8sClient = r.Client

	meta.TerraformImage = os.Getenv("TERRAFORM_IMAGE")
	if meta.TerraformImage == "" {
		meta.TerraformImage = "oamdev/docker-terraform:1.1.2"
	}

	meta.TerraformBackendNamespace = os.Getenv("TERRAFORM_BACKEND_NAMESPACE")
	if meta.TerraformBackendNamespace == "" {
		meta.TerraformBackendNamespace = "vela-system"
	}

	meta.BusyboxImage = os.Getenv("BUSYBOX_IMAGE")
	if meta.BusyboxImage == "" {
		meta.BusyboxImage = "busybox:latest"
	}
	meta.GitImage = os.Getenv("GIT_IMAGE")
	if meta.GitImage == "" {
		meta.GitImage = "alpine/git:latest"
	}

	// Validation: 1) validate Configuration itself
	configurationType, err := tfcfg.ValidConfigurationObject(configuration)
	if err != nil {
		if updateErr := meta.updateApplyStatus(ctx, k8sClient, types.ConfigurationStaticCheckFailed, err.Error()); updateErr != nil {
			return updateErr
		}
		return err
	}
	meta.ConfigurationType = configurationType

	// TODO(zzxwill) Need to find an alternative to check whether there is an state backend in the Configuration

	// Render configuration with backend
	completeConfiguration, err := tfcfg.RenderConfiguration(configuration, meta.TerraformBackendNamespace, configurationType)
	if err != nil {
		return err
	}
	meta.CompleteConfiguration = completeConfiguration

	if err := meta.storeTFConfiguration(ctx, k8sClient); err != nil {
		return err
	}

	// Check whether configuration(hcl/json) is changed
	if err := meta.CheckWhetherConfigurationChanges(ctx, k8sClient, configurationType); err != nil {
		return err
	}

	if meta.ConfigurationChanged {
		klog.InfoS("Configuration hanged, reloading...")
		if err := meta.updateApplyStatus(ctx, k8sClient, types.ConfigurationReloading, types.ConfigurationReloadingAsHCLChanged); err != nil {
			return err
		}
		// store configuration to ConfigMap
		return meta.storeTFConfiguration(ctx, k8sClient)
	}

	// Check provider
	if len(meta.ProviderReference.Namespace) == 0 {
		meta.ProviderReference.Namespace = configuration.Namespace
	}
	provider, err := provider.GetProviderFromConfiguration(ctx, k8sClient, meta.ProviderReference.Namespace, meta.ProviderReference.Name)
	if provider == nil {
		msg := types.ErrProviderNotFound
		if err != nil {
			msg = err.Error()
		}
		message := stripansi.Strip(msg)
		if updateStatusErr := meta.updateApplyStatus(ctx, k8sClient, types.Authorizing, message); updateStatusErr != nil {
			return errors.Wrap(updateStatusErr, message)
		}
		return errors.New(message)
	}

	//TODO check here
	if err := meta.getCredentials(ctx, k8sClient, provider); err != nil {
		return err
	}

	// Apply ClusterRole
	return createTerraformExecutorClusterRole(ctx, k8sClient, fmt.Sprintf("%s-%s", meta.Namespace, ClusterRoleName))
}

func (meta *TFConfigurationMeta) updateApplyStatus(ctx context.Context, k8sClient client.Client, state types.ConfigurationState, message string) error {
	var configuration v1beta1.Configuration
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.Name, Namespace: meta.Namespace}, &configuration); err == nil {

		var phase types.Phase
		if state == types.Available {
			phase = types.ApplySuccessfull
		} else if state == types.ConfigurationApplyFailed {
			phase = types.ApplyFailed
		} else if configuration.Status.Apply.Phase != "" {
			phase = configuration.Status.Apply.Phase
		} else {
			phase = types.ApplyPending
		}

		if !(phase == types.ApplyFailed && state == types.ConfigurationProvisioningAndChecking && message == types.MessageCloudResourceProvisioningAndChecking) {
			configuration.Status.Apply = v1beta1.ConfigurationApplyStatus{
				State:   state,
				Message: stripansi.Strip(message),
			}
		}

		if state == types.Available {
			outputs, err := meta.getTFOutputs(ctx, k8sClient, configuration)
			if err != nil {
				configuration.Status.Apply = v1beta1.ConfigurationApplyStatus{
					State:   types.GeneratingOutputs,
					Message: stripansi.Strip(types.ErrGenerateOutputs + ": " + err.Error()),
				}
			} else {
				configuration.Status.Apply.Outputs = outputs
			}
		}
		configuration.Status.Apply.Phase = phase

		return k8sClient.Status().Update(ctx, &configuration)
	}
	return nil
}

func (meta *TFConfigurationMeta) updateDestroyStatus(ctx context.Context, k8sClient client.Client, state types.ConfigurationState, message string) error {
	var configuration v1beta1.Configuration
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.Name, Namespace: meta.Namespace}, &configuration); err == nil {
		configuration.Status.Destroy = v1beta1.ConfigurationDestroyStatus{
			State:   state,
			Message: stripansi.Strip(message),
		}
		return k8sClient.Status().Update(ctx, &configuration)
	}
	return nil
}

func (meta *TFConfigurationMeta) assembleAndTriggerJob(ctx context.Context, k8sClient client.Client,
	configuration *v1beta1.Configuration, executionType TerraformExecutionType) error {

	// apply rbac
	if err := createTerraformExecutorServiceAccount(ctx, k8sClient, meta.Namespace, ServiceAccountName); err != nil {
		return err
	}
	if err := createTerraformExecutorClusterRoleBinding(ctx, k8sClient, meta.Namespace, fmt.Sprintf("%s-%s", meta.Namespace, ClusterRoleName), ServiceAccountName); err != nil {
		return err
	}

	if err := meta.prepareTFVariables(configuration, k8sClient); err != nil {
		return err
	}

	if err := meta.updateTerraformJobSecretIfNeeded(ctx, k8sClient); err != nil {
		return err
	}

	job := meta.assembleTerraformJob(executionType, configuration)

	if err := meta.updateApplyStatus(ctx, k8sClient, types.ConfigurationProvisioningAndChecking, types.MessageCloudResourceProvisioningAndChecking); err != nil {
		return err
	}

	return k8sClient.Create(ctx, job)
}

func (meta *TFConfigurationMeta) updateTerraformJobSecretIfNeeded(ctx context.Context, k8sClient client.Client) error {
	sec := v1.Secret{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.VariableSecretName, Namespace: meta.Namespace}, &sec); err != nil {
		if kerrors.IsNotFound(err) {
			var secret = v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      meta.VariableSecretName,
					Namespace: meta.Namespace,
					Annotations: map[string]string{
						VariableSecretHashAnnotationKey: util.GenerateHash(meta.VariableSecretData),
					},
				},
				TypeMeta: metav1.TypeMeta{Kind: "Secret"},
				Data:     meta.VariableSecretData,
			}
			if err := k8sClient.Create(ctx, &secret); err != nil {
				return err
			}
		}
	} else {
		for k, v := range meta.VariableSecretData {
			if sec.Data == nil {
				sec.Data = make(map[string][]byte)
			}
			sec.Data[k] = v
		}
		if sec.Annotations == nil {
			sec.Annotations = make(map[string]string)
		}
		sec.Annotations[VariableSecretHashAnnotationKey] = util.GenerateHash(sec.Data)
		if err := k8sClient.Update(ctx, &sec); err != nil {
			return err
		}
	}
	return nil
}

// updateTerraformJob will set deletion finalizer to the Terraform job if its envs are changed, which will result in
// deleting the job. Finally, a new Terraform job will be generated
func (meta *TFConfigurationMeta) updateTerraformJobIfNeeded(ctx context.Context, k8sClient client.Client, configuration v1beta1.Configuration,
	job batchv1.Job) error {
	if err := meta.prepareTFVariables(&configuration, k8sClient); err != nil {
		return err
	}

	// check whether env changes
	var variableInSecret v1.Secret
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.VariableSecretName, Namespace: meta.Namespace}, &variableInSecret); err != nil {
		return err
	}

	var envChanged bool
	// for k, v := range variableInSecret.Data {
	// 	if val, ok := meta.VariableSecretData[k]; ok {
	// 		if (string(val) != "FROM_SECRET_REF" && !bytes.Equal(v, val)) ||
	// 			(variableInSecret.Annotations[VariableSecretHashAnnotationKey] != util.GenerateHash(meta.VariableSecretData)) {
	// 			envChanged = true
	// 			klog.Info(fmt.Sprintf("Job's env changed %s %s, %s", k, v, val))
	// 			if err := meta.updateApplyStatus(ctx, k8sClient, types.ConfigurationReloading, types.ConfigurationReloadingAsVariableChanged); err != nil {
	// 				return err
	// 			}
	// 		}
	// 	}
	// }

	// if either one changes, delete the job
	if envChanged || meta.ConfigurationChanged {
		klog.InfoS("about to delete job", "Name", job.Name, "Namespace", job.Namespace)
		var j batchv1.Job
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: job.Name, Namespace: job.Namespace}, &j); err == nil {
			if deleteErr := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil {
				return deleteErr
			}
		}
		if meta.DeleteSecretOnJobUpdate {
			var s v1.Secret
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.VariableSecretName, Namespace: meta.Namespace}, &s); err == nil {
				if deleteErr := k8sClient.Delete(ctx, &s); deleteErr != nil {
					return deleteErr
				}
			}
		} else {
			if err := meta.updateTerraformJobSecretIfNeeded(ctx, k8sClient); err != nil {
				klog.ErrorS(err, types.ErrUpdateTerraformApplyJob, "Name", meta.ApplyJobName)
				return errors.Wrap(err, types.ErrUpdateTerraformApplyJob)
			}
		}
	}
	return nil
}

// Adding support for sleep so that issues can be debugged easily by logging in
func (meta *TFConfigurationMeta) getExecutableContainerCommand(executionType TerraformExecutionType) []string {
	if len(os.Getenv("SLEEP")) > 0 {
		return []string{
			"bash",
			"-c",
			fmt.Sprintf("sleep %s; /providers/exec.sh %s", os.Getenv("SLEEP"), executionType),
		}
	} else {
		return []string{
			"bash",
			"-c",
			fmt.Sprintf("/providers/exec.sh %s", executionType),
		}
	}
}

func (meta *TFConfigurationMeta) assembleTerraformJob(executionType TerraformExecutionType, configuration *v1beta1.Configuration) *batchv1.Job {
	var (
		initContainer  v1.Container
		initContainers []v1.Container
		parallelism    int32 = 1
		completions    int32 = 1
		backoffLimit   int32 = math.MaxInt32
	)

	executorVolumes := meta.assembleExecutorVolumes(configuration)
	initContainerVolumeMounts := append([]v1.VolumeMount{
		{
			Name:      meta.Name,
			MountPath: WorkingVolumeMountPath,
		},
		{
			Name:      InputTFConfigurationVolumeName,
			MountPath: InputTFConfigurationVolumeMountPath,
		},
		{
			Name:      BackendVolumeName,
			MountPath: BackendVolumeMountPath,
		},
	}, configuration.Spec.VolumeSpec.VolumeMounts...)

	initContainer = v1.Container{
		Name:            "prepare-input-terraform-configurations",
		Image:           meta.BusyboxImage,
		ImagePullPolicy: v1.PullIfNotPresent,
		Command: []string{
			"sh",
			"-c",
			fmt.Sprintf("cp %s/* %s", InputTFConfigurationVolumeMountPath, WorkingVolumeMountPath),
		},
		VolumeMounts: initContainerVolumeMounts,
	}
	initContainers = append(initContainers, initContainer)

	hclPath := filepath.Join(BackendVolumeMountPath, meta.RemoteGitPath)

	if meta.RemoteGit != "" {
		initContainers = append(initContainers,
			v1.Container{
				Name:            "git-configuration",
				Image:           meta.GitImage,
				ImagePullPolicy: v1.PullIfNotPresent,
				Command: []string{
					"sh",
					"-c",
					fmt.Sprintf("git clone %s %s && cp -r %s/* %s", meta.RemoteGit, BackendVolumeMountPath,
						hclPath, WorkingVolumeMountPath),
				},
				VolumeMounts: initContainerVolumeMounts,
			})
	}

	//- name: spectro-common-dev-image-pull-secret
	//- name: spectro-image-pull-secret

	pullSecrets := make([]v1.LocalObjectReference, 0, 1)
	pullSecrets = append(pullSecrets, v1.LocalObjectReference{Name: "spectro-common-dev-image-pull-secret"})
	pullSecrets = append(pullSecrets, v1.LocalObjectReference{Name: "spectro-image-pull-secret"})

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Job",
			APIVersion: "batch/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      meta.Name + "-" + string(executionType),
			Namespace: meta.Namespace,
		},
		Spec: batchv1.JobSpec{
			Parallelism:  &parallelism,
			Completions:  &completions,
			BackoffLimit: &backoffLimit,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"spectrocloud.com/connection": "proxy",
						"spectrocloud.com/monitor":    "skip",
					},
				},
				Spec: v1.PodSpec{
					ImagePullSecrets: pullSecrets,
					// InitContainer will copy Terraform configuration files to working directory and create Terraform
					// state file directory in advance
					InitContainers: initContainers,
					// Container terraform-executor will first copy predefined terraform.d to working directory, and
					// then run terraform init/apply.
					Containers: []v1.Container{{
						Name:            "terraform-executor",
						Image:           meta.TerraformImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Command:         meta.getExecutableContainerCommand(executionType),

						VolumeMounts: append([]v1.VolumeMount{
							{
								Name:      meta.Name,
								MountPath: WorkingVolumeMountPath,
							},
							{
								Name:      InputTFConfigurationVolumeName,
								MountPath: InputTFConfigurationVolumeMountPath,
							},
						}, configuration.Spec.VolumeSpec.VolumeMounts...),
						Env: meta.Envs,
					},
					},
					ServiceAccountName: ServiceAccountName,
					Volumes:            executorVolumes,
					RestartPolicy:      v1.RestartPolicyOnFailure,
				},
			},
		},
	}
}

func (meta *TFConfigurationMeta) getPrePostExecCommandEnvs(configuration *v1beta1.Configuration) []v1.EnvVar {
	envVar := []v1.EnvVar{}
	if configuration.Spec.PreExecCommand != nil {
		preExecCmd := v1.EnvVar{
			Name:  "PREEXECCMD",
			Value: *configuration.Spec.PreExecCommand,
		}

		envVar = append(envVar, preExecCmd)
	}
	if configuration.Spec.PostExecCommand != nil {
		preExecCmd := v1.EnvVar{
			Name:  "POSTEXECCMD",
			Value: *configuration.Spec.PostExecCommand,
		}

		envVar = append(envVar, preExecCmd)
	}
	return envVar
}

func (meta *TFConfigurationMeta) assembleExecutorVolumes(configuration *v1beta1.Configuration) []v1.Volume {
	workingVolume := v1.Volume{Name: meta.Name}
	workingVolume.EmptyDir = &v1.EmptyDirVolumeSource{}
	inputTFConfigurationVolume := meta.createConfigurationVolume()
	tfBackendVolume := meta.createTFBackendVolume()
	return append([]v1.Volume{workingVolume, inputTFConfigurationVolume, tfBackendVolume}, configuration.Spec.VolumeSpec.Volumes...)
}

func (meta *TFConfigurationMeta) createConfigurationVolume() v1.Volume {
	inputCMVolumeSource := v1.ConfigMapVolumeSource{}
	inputCMVolumeSource.Name = meta.ConfigurationCMName
	inputTFConfigurationVolume := v1.Volume{Name: InputTFConfigurationVolumeName}
	inputTFConfigurationVolume.ConfigMap = &inputCMVolumeSource
	return inputTFConfigurationVolume

}

func (meta *TFConfigurationMeta) createTFBackendVolume() v1.Volume {
	gitVolume := v1.Volume{Name: BackendVolumeName}
	gitVolume.EmptyDir = &v1.EmptyDirVolumeSource{}
	return gitVolume
}

// TFState is Terraform State
type TFState struct {
	Outputs map[string]v1beta1.Property `json:"outputs"`
}

//nolint:funlen
func (meta *TFConfigurationMeta) getTFOutputs(ctx context.Context, k8sClient client.Client, configuration v1beta1.Configuration) (map[string]v1beta1.Property, error) {
	var s = v1.Secret{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.BackendSecretName, Namespace: meta.TerraformBackendNamespace}, &s); err != nil {
		return nil, errors.Wrap(err, "terraform state file backend secret is not generated")
	}
	tfStateData, ok := s.Data[TerraformStateNameInSecret]
	if !ok {
		return nil, fmt.Errorf("failed to get %s from Terraform State secret %s", TerraformStateNameInSecret, s.Name)
	}

	tfStateJSON, err := util.DecompressTerraformStateSecret(string(tfStateData))
	if err != nil {
		return nil, errors.Wrap(err, "failed to decompress state secret data")
	}

	var tfState TFState
	if err := json.Unmarshal(tfStateJSON, &tfState); err != nil {
		return nil, err
	}

	outputs := tfState.Outputs
	writeConnectionSecretToReference := configuration.Spec.WriteConnectionSecretToReference
	if writeConnectionSecretToReference == nil || writeConnectionSecretToReference.Name == "" {
		return outputs, nil
	}

	name := writeConnectionSecretToReference.Name
	ns := writeConnectionSecretToReference.Namespace
	if ns == "" {
		ns = configuration.Namespace
	}
	data := make(map[string][]byte)
	for k, v := range outputs {
		data[k] = []byte(v.Value)
	}
	var gotSecret v1.Secret
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &gotSecret); err != nil {
		if kerrors.IsNotFound(err) {
			var secret = v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: ns,
					Labels: map[string]string{
						"created-by": "terraform-controller",
					},
				},
				TypeMeta: metav1.TypeMeta{Kind: "Secret"},
				Data:     data,
			}
			if err := k8sClient.Create(ctx, &secret); err != nil {
				return nil, err
			}
		}
	} else {
		gotSecret.Data = data
		if err := k8sClient.Update(ctx, &gotSecret); err != nil {
			return nil, err
		}
	}
	return outputs, nil
}

func (meta *TFConfigurationMeta) prepareTFVariables(configuration *v1beta1.Configuration, k8sClient client.Client) error {
	var (
		envs []v1.EnvVar
		data = map[string][]byte{}
	)

	if configuration == nil {
		return errors.New("configuration is nil")
	}
	if meta.ProviderReference == nil {
		return errors.New("The referenced provider could not be retrieved")
	}

	if configuration.Spec.VariableRef != nil {
		envs = configuration.Spec.VariableRef
	}

	tfVariable, err := getTerraformJSONVariable(configuration.Spec.Variable)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to get Terraform JSON variables from Configuration Variables %v", configuration.Spec.Variable))
	}

	skipNotFoundErr := true
	for _, v := range tfVariable {
		envValue, err := tfcfg.Interface2String(v)
		if err != nil {
			return err
		}
		if envValue == "FROM_SECRET_REF" {
			skipNotFoundErr = false
		}
	}

	secretData := make(map[string][]byte)
	var variableInSecret v1.Secret
	if err := k8sClient.Get(context.TODO(), client.ObjectKey{Name: meta.VariableSecretName, Namespace: meta.Namespace}, &variableInSecret); err != nil {
		if !skipNotFoundErr && kerrors.IsNotFound(err) {
			return err
		}
	} else {
		if len(variableInSecret.Data) > 0 {
			secretData = variableInSecret.Data
		}
	}

	//get secret if it exists and add it in env var
	for k, v := range tfVariable {
		envValue, err := tfcfg.Interface2String(v)
		if err != nil {
			return err
		}
		if envValue != "FROM_SECRET_REF" {
			data[k] = []byte(envValue)
		} else {
			meta.DeleteSecretOnJobUpdate = false
			data[k] = secretData[k]
		}

		valueFrom := &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Key: k}}
		valueFrom.SecretKeyRef.Name = meta.VariableSecretName
		envs = append(envs, v1.EnvVar{Name: k, ValueFrom: valueFrom})
	}

	for k, v := range meta.Credentials {
		data[k] = []byte(v)
		valueFrom := &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Key: k}}
		valueFrom.SecretKeyRef.Name = meta.VariableSecretName
		envs = append(envs, v1.EnvVar{Name: k, ValueFrom: valueFrom})
	}

	if prePostEnvs := meta.getPrePostExecCommandEnvs(configuration); len(prePostEnvs) != 0 {
		envs = append(envs, prePostEnvs...)
	}

	job_status := v1.EnvVar{
		Name:  "JOB_STATUS",
		Value: "ApplyPending",
	}
	if configuration.Status.Apply.Phase != "" {
		job_status.Value = string(configuration.Status.Apply.Phase)
	}

	envs = append(envs, job_status)

	meta.Envs = envs
	meta.VariableSecretData = data

	return nil
}

// SetupWithManager setups with a manager
func (r *ConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Configuration{}).
		Complete(r)
}

func getTerraformJSONVariable(tfVariables *runtime.RawExtension) (map[string]interface{}, error) {
	variables, err := tfcfg.RawExtension2Map(tfVariables)
	if err != nil {
		return nil, err
	}
	var environments = make(map[string]interface{})

	for k, v := range variables {
		environments[fmt.Sprintf("TF_VAR_%s", k)] = v
	}
	return environments, nil
}

func (meta *TFConfigurationMeta) deleteConfigMap(ctx context.Context, k8sClient client.Client) error {
	var cm v1.ConfigMap
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.ConfigurationCMName, Namespace: meta.Namespace}, &cm); err == nil {
		if err := k8sClient.Delete(ctx, &cm); err != nil {
			return err
		}
	}
	return nil
}

func deleteConnectionSecret(ctx context.Context, k8sClient client.Client, name, ns string) error {
	if len(name) == 0 {
		return nil
	}

	var connectionSecret v1.Secret
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &connectionSecret); err == nil {
		return k8sClient.Delete(ctx, &connectionSecret)
	}
	return nil
}

func (meta *TFConfigurationMeta) createOrUpdateConfigMap(ctx context.Context, k8sClient client.Client, data map[string]string) error {
	var gotCM v1.ConfigMap
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.ConfigurationCMName, Namespace: meta.Namespace}, &gotCM); err != nil {
		if kerrors.IsNotFound(err) {
			cm := v1.ConfigMap{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      meta.ConfigurationCMName,
					Namespace: meta.Namespace,
				},
				Data: data,
			}
			err := k8sClient.Create(ctx, &cm)
			return errors.Wrap(err, "failed to create TF configuration ConfigMap")
		}
		return err
	}
	if !reflect.DeepEqual(gotCM.Data, data) {
		gotCM.Data = data
		return errors.Wrap(k8sClient.Update(ctx, &gotCM), "failed to update TF configuration ConfigMap")
	}
	return nil
}

func (meta *TFConfigurationMeta) prepareTFInputConfigurationData() map[string]string {
	var dataName string
	switch meta.ConfigurationType {
	case types.ConfigurationJSON:
		dataName = types.TerraformJSONConfigurationName
	case types.ConfigurationHCL:
		dataName = types.TerraformHCLConfigurationName
	case types.ConfigurationRemote:
		dataName = "terraform-backend.tf"
	}
	data := map[string]string{dataName: meta.CompleteConfiguration, "kubeconfig": ""}
	return data
}

// storeTFConfiguration will store Terraform configuration to ConfigMap
func (meta *TFConfigurationMeta) storeTFConfiguration(ctx context.Context, k8sClient client.Client) error {
	data := meta.prepareTFInputConfigurationData()
	return meta.createOrUpdateConfigMap(ctx, k8sClient, data)
}

// CheckWhetherConfigurationChanges will check whether configuration is changed
func (meta *TFConfigurationMeta) CheckWhetherConfigurationChanges(ctx context.Context, k8sClient client.Client, configurationType types.ConfigurationType) error {
	var cm v1.ConfigMap
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: meta.ConfigurationCMName, Namespace: meta.Namespace}, &cm); err != nil {
		return err
	}

	var configurationChanged bool
	switch configurationType {
	case types.ConfigurationJSON:
		meta.ConfigurationChanged = true
		return nil
	case types.ConfigurationHCL:
		configurationChanged = cm.Data[types.TerraformHCLConfigurationName] != meta.CompleteConfiguration
		meta.ConfigurationChanged = configurationChanged
		if configurationChanged {
			klog.InfoS("Configuration HCL changed", "ConfigMap", cm.Data[types.TerraformHCLConfigurationName],
				"RenderedCompletedConfiguration", meta.CompleteConfiguration)
		}

		return nil
	case types.ConfigurationRemote:
		meta.ConfigurationChanged = false
		return nil
	}

	return errors.New("unknown issue")
}

// getCredentials will get credentials from secret of the Provider
func (meta *TFConfigurationMeta) getCredentials(ctx context.Context, k8sClient client.Client,
	providerObj *v1beta1.Provider) error {
	region, err := tfcfg.SetRegion(ctx, k8sClient, meta.Namespace, meta.Name, providerObj)
	if err != nil {
		return err
	}

	credentials, err := provider.GetProviderCredentials(ctx, k8sClient, providerObj, region)
	if err != nil {
		return err
	}
	meta.Credentials = credentials
	return nil
}

//apiVersion: v1
//data:
//TF_VAR_NETWORK_WAN: dnRuZXQw
//TF_VAR_NETWORK_LAN: dnRuZXQx
//TF_VAR_IP_ADDR_WAN: ZGhjcA==
//TF_VAR_IP_ADDR_LAN: MTkyLjE2OC4xMDAuMg==
//TF_VAR_SUBNET_WAN: ""
//TF_VAR_SUBNET_LAN: MjQ=
//TF_VAR_DHCP_RANGE_START: MTkyLjE2OC4xMDAuNTA=
//TF_VAR_DHCP_RANGE_END: MTkyLjE2OC4xMDAuMjAw
//TF_VAR_VM_NAME: cGZzZW5zZS12bS1vcGVyYXRvcg==
//TF_VAR_HOST_IP: My43MC4yMTAuNA==
//TF_VAR_SSH_USER: cm9vdA==
//TF_VAR_SSH_KEY: L3Zhci9rZXlzL3NzaC1rZXk=
//kind: Secret
//metadata:
//name: variable-custom-example
//namespace: default
//type: Opaque
