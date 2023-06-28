/*
Copyright 2022.

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
	fdsev1alpha1 "deep-yellow/petition/api/v1alpha1"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"
)

// 使用petition的名字作为云端的NameSpace
var nameSpaceOnCloud string

// PetitionReconciler reconciles a Petition object
type PetitionReconciler struct {
	client.Client
	Log               logr.Logger
	Scheme            *runtime.Scheme
	Recorder          record.EventRecorder
	PodInformer       cache.SharedIndexInformer
	ConfigMapInformer cache.SharedIndexInformer
	SecretInformer    cache.SharedIndexInformer
}

//+kubebuilder:rbac:groups=fdse.meixiezichuan,resources=petitions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fdse.meixiezichuan,resources=petitions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=fdse.meixiezichuan,resources=petitions/finalizers,verbs=update
//+kubebuilder:rbac:groups=fdse.meixiezichuan,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fdse.meixiezichuan,resources=pods/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Petition object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.tition
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.1/pkg/reconcile
func (r *PetitionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// get petition instance
	var p fdsev1alpha1.Petition
	err := r.Get(ctx, req.NamespacedName, &p)
	if err != nil {
		if errors.IsNotFound(err) {
			r.Log.Info("CR Petition not exist")
			return ctrl.Result{}, nil
		}
		r.Log.Error(err, "error on getting cr petition")
		return ctrl.Result{}, err
	}
	// check all pod
	podList := &v1.PodList{}
	opts := []client.ListOption{
		client.MatchingFields{"status.phase": "Pending"},
	}
	listErr := r.List(ctx, podList, opts...)
	if listErr != nil {
		return ctrl.Result{}, listErr
	}
	// get cloud client
	// send scheduling request to remote server
	config, er := clientcmd.BuildConfigFromFlags("", p.Spec.CloudConfig)
	if er != nil {
		r.Log.Error(er, "error on getting config from CloudConfig")
		return ctrl.Result{}, er
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	// TODO 两个client对象作用一样用法不同，想办法统一下
	cloudClient, er1 := client.New(config, client.Options{Scheme: scheme})
	cloudCli, _ := kubernetes.NewForConfig(config)
	if er1 != nil {
		r.Log.Error(er1, "error on getting cloud client from CloudConfig")
		return ctrl.Result{}, er1
	}
	var errList []error

	// set namespace
	nameSpaceOnCloud = getNameSpace(&p)
	if nameSpaceOnCloud == "" {
		r.Log.Info("Cannot get petition-name ")
		return ctrl.Result{}, nil
	}
	// create namespace
	if err := r.createNameSpace(ctx, cloudCli, nameSpaceOnCloud); err != nil {
		errList = append(errList, err)
	}

	for _, pod := range podList.Items {
		reSchedule := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == v1.PodScheduled && condition.Reason == "Unschedulable" {

				r.Log.Info("Found Pod in pending state, Pod name: " + pod.Name)

				// 判断deploy是否已创建
				deploy := &appsv1.Deployment{}
				err := cloudClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: nameSpaceOnCloud}, deploy)
				if err != nil {
					//没找到deploy
					reSchedule = true
				} else {
					if ready, _ := r.isDeploymentReady(ctx, cloudClient, deploy); ready {
						r.Log.Info("Deployment ready in cloud cluster, deleting local pod")
						_ = r.Delete(ctx, &pod)
						r.Log.Info("Finished.")
					} else {
						r.Log.Info("Deploy not prepared yet.")
						return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
					}
				}
				break
			}
		}
		if reSchedule {
			// create relevant resources
			if _, err := r.createConfigMapAndSecret(ctx, cloudClient, &pod); err != nil {
				errList = append(errList, err)
			}

			// create deployment and service on cloud
			_, er2 := r.CreateDeploymentAndService(ctx, cloudClient, &pod)
			if er2 != nil {
				errList = append(errList, er2)
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	if len(errList) > 0 {
		return ctrl.Result{}, errList[0]
	}
	return ctrl.Result{}, nil
}

func (r *PetitionReconciler) CreateDeploymentAndService(ctx context.Context, cli client.Client, pod *v1.Pod) (ctrl.Result, error) {
	var err error
	var dp appsv1.Deployment
	var replica int32 = 1
	podLabels := map[string]string{"app": pod.Name}
	dp.Namespace = nameSpaceOnCloud
	dp.Name = pod.Name
	dp.Spec.Replicas = &replica
	dp.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: podLabels}
	dp.Spec.Template.Labels = podLabels
	dp.Spec.Template.Spec = *pod.Spec.DeepCopy()
	err = cli.Create(ctx, &dp)
	if err != nil {
		return ctrl.Result{}, err
	}

	// if pod has port exposed, create a service
	if len(pod.Spec.Containers[0].Ports) > 0 {
		var svc v1.Service
		svc.Name = pod.Name
		svc.Namespace = nameSpaceOnCloud
		svc.Spec.Selector = podLabels
		for _, p := range pod.Spec.Containers[0].Ports {
			svcPort := v1.ServicePort{
				Port:     p.ContainerPort,
				Protocol: p.Protocol,
				Name:     p.Name,
			}
			svc.Spec.Ports = append(svc.Spec.Ports, svcPort)
		}
		err = cli.Create(ctx, &svc)
	}

	return ctrl.Result{}, err
}

// 创建相关资源，现在只考虑configmap和secret
func (r *PetitionReconciler) createConfigMapAndSecret(ctx context.Context, cloudCli client.Client, pod *v1.Pod) (ctrl.Result, error) {
	configMaps, err := r.getRelatedConfigMaps(ctx, pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.createConfigMaps(ctx, cloudCli, configMaps); err != nil {
		return ctrl.Result{}, err
	}

	secrets, err := r.getRelatedSecrets(ctx, pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.createSecrets(ctx, cloudCli, secrets); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// 找到pod相关ConfigMap
func (r *PetitionReconciler) getRelatedConfigMaps(ctx context.Context, pod *v1.Pod) ([]*v1.ConfigMap, error) {

	//Volumes
	configMaps := make(map[string]*v1.ConfigMap)
	for _, volume := range pod.Spec.Volumes {
		if volume.ConfigMap != nil {
			cm := &v1.ConfigMap{}
			err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: volume.ConfigMap.Name}, cm)
			if err != nil {
				return nil, err
			}
			configMaps[cm.Name] = cm
		}
	}

	//Env
	for _, env := range pod.Spec.Containers[0].Env {
		if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
			configMap := &v1.ConfigMap{}
			err := r.Get(context.Background(), types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      env.ValueFrom.ConfigMapKeyRef.Name,
			}, configMap)
			if err != nil {
				return nil, err
			}
			configMaps[configMap.Name] = configMap
		}
	}

	var result []*v1.ConfigMap
	for _, cm := range configMaps {
		result = append(result, cm)
	}
	return result, nil
}

// 找到pod相关的Secret
func (r *PetitionReconciler) getRelatedSecrets(ctx context.Context, pod *v1.Pod) ([]*v1.Secret, error) {
	var secrets []*v1.Secret
	secretMap := make(map[string]*v1.Secret)

	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil {
			secret := &v1.Secret{}
			err := r.Get(ctx, types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      volume.Secret.SecretName,
			}, secret)
			if err != nil {
				return nil, err
			}
			secretMap[secret.Namespace+"/"+secret.Name] = secret
		}
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			secret := &v1.Secret{}
			err := r.Get(ctx, types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      env.ValueFrom.SecretKeyRef.Name,
			}, secret)
			if err != nil {
				return nil, err
			}
			secretMap[secret.Namespace+"/"+secret.Name] = secret
		}
	}

	for _, secret := range secretMap {
		secrets = append(secrets, secret)
	}

	return secrets, nil
}

// 遍历列表创建ConfigMap
func (r *PetitionReconciler) createConfigMaps(ctx context.Context, cli client.Client, configMaps []*v1.ConfigMap) error {
	for _, cm := range configMaps {
		newCM := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cm.Name,
				Namespace: nameSpaceOnCloud,
			},
			Data: cm.Data,
		}
		err := cli.Create(ctx, newCM)
		if err != nil {
			return err
		}
	}
	return nil
}

// 遍历列表创建Secret
func (r *PetitionReconciler) createSecrets(ctx context.Context, cli client.Client, secrets []*v1.Secret) error {
	for _, secret := range secrets {
		newSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secret.Name,
				Namespace: nameSpaceOnCloud,
			},
			Data: secret.Data,
			Type: secret.Type,
		}
		err := cli.Create(ctx, newSecret)
		if err != nil {
			return err
		}
	}
	return nil
}

// 判断一下有没有对应的namespace，没有创建一个
func (r *PetitionReconciler) createNameSpace(ctx context.Context, cli *kubernetes.Clientset, namespace string) error {
	_, err := cli.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		ns := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		_, err2 := cli.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		if err2 != nil {
			return err2
		}
	} else if err != nil {
		return err
	}
	return nil
}

// 检查deploy管理的所有pod是否都为running
func (r *PetitionReconciler) isDeploymentReady(ctx context.Context, cloudCli client.Client, deploy *appsv1.Deployment) (bool, error) {
	// 获取 Deployment 下的所有 Pod
	podList := &v1.PodList{}
	err := cloudCli.List(ctx, podList, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(deploy.Spec.Selector.MatchLabels),
		Namespace:     deploy.Namespace,
	})
	if err != nil {
		return false, err
	}
	// 判断所有 Pod 的状态是否都为 Running
	for _, pod := range podList.Items {
		if pod.Status.Phase != v1.PodRunning {
			return false, nil
		}
	}
	return true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PetitionReconciler) SetupWithManager(mgr ctrl.Manager) error {

	localCli := kubernetes.NewForConfigOrDie(mgr.GetConfig())
	sharedInformers := informers.NewSharedInformerFactoryWithOptions(localCli, time.Minute*10, informers.WithNamespace("default"))

	r.PodInformer = sharedInformers.Core().V1().Pods().Informer()
	r.ConfigMapInformer = sharedInformers.Core().V1().ConfigMaps().Informer()
	r.SecretInformer = sharedInformers.Core().V1().Secrets().Informer()

	cloudCli := fakeCloudCli()
	r.PodInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*v1.Pod)
			pod.ResourceVersion = ""
			addLabelAndSelector(pod)
			_, err := cloudCli.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				klog.Warningf("Failed to report pod created, namespace: %s, name: %s, err: %v", pod.Namespace, pod.Name, err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPod := newObj.(*v1.Pod)
			addLabelAndSelector(newPod)
			_, err := cloudCli.CoreV1().Pods(newPod.Namespace).Update(context.Background(), newPod, metav1.UpdateOptions{})
			if err != nil && !errors.IsNotFound(err) {
				klog.Warningf("Failed to find and update pod, namespace: %s, name: %s, err: %v", newPod.Namespace, newPod.Name, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*v1.Pod)
			addLabelAndSelector(pod)
			err := cloudCli.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) && !strings.Contains(err.Error(), "The object might have been deleted and then recreated") {
				klog.Warningf("Failed to delete pod, namespace: %s, name: %s, err: %v", pod.Namespace, pod.Name, err)
			}
		},
	})

	r.ConfigMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cm := obj.(*v1.ConfigMap)
			cm.ResourceVersion = ""
			_, err := cloudCli.CoreV1().ConfigMaps(cm.Namespace).Create(context.Background(), cm, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				klog.Warningf("Failed to create configmap, namespace: %s, name: %s, err: %v", cm.Namespace, cm.Name, err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cm := newObj.(*v1.ConfigMap)
			_, err := cloudCli.CoreV1().ConfigMaps(cm.Namespace).Update(context.Background(), cm, metav1.UpdateOptions{})
			if err != nil && !errors.IsNotFound(err) {
				klog.Warningf("Failed to find and update configmap, namespace: %s, name: %s, err: %v", cm.Namespace, cm.Name, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			cm := obj.(*v1.ConfigMap)
			err := cloudCli.CoreV1().ConfigMaps(cm.Namespace).Delete(context.Background(), cm.Name, metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				klog.Warningf("Failed to delete configmap, namespace: %s, name: %s, err: %v", cm.Namespace, cm.Name, err)
			}
		},
	})

	r.SecretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			secret := obj.(*v1.Secret)
			secret.ResourceVersion = ""
			_, err := cloudCli.CoreV1().Secrets(secret.Namespace).Create(context.Background(), secret, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				klog.Warningf("Failed to create secret, namespace: %s, name: %s, err: %v", secret.Namespace, secret.Name, err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			secret := newObj.(*v1.Secret)
			_, err := cloudCli.CoreV1().Secrets(secret.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{})
			if err != nil && !errors.IsNotFound(err) {
				klog.Warningf("Failed to find and update secret, namespace: %s, name: %s, err: %v", secret.Namespace, secret.Name, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			secret := obj.(*v1.Secret)
			err := cloudCli.CoreV1().Secrets(secret.Namespace).Delete(context.Background(), secret.Name, metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				klog.Warningf("Failed to delete secret, namespace: %s, name: %s, err: %v", secret.Namespace, secret.Name, err)
			}
		},
	})

	stopCh := make(chan struct{})
	go r.PodInformer.Run(stopCh)
	go r.ConfigMapInformer.Run(stopCh)
	go r.SecretInformer.Run(stopCh)

	// add index to pod.status.phase
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1.Pod{}, "status.phase", func(rawObj client.Object) []string {
		pod := rawObj.(*v1.Pod)
		return []string{string(pod.Status.Phase)}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&fdsev1alpha1.Petition{}).
		Owns(&v1.Pod{}).
		Owns(&v1.ConfigMap{}).
		Owns(&v1.Secret{}).
		Complete(r)
}

// 当前直接设置Namespace为petition的name，可能会变更
func getNameSpace(petition *fdsev1alpha1.Petition) string {
	return petition.Name
}

func fakeCloudCli() kubernetes.Clientset {
	config, _ := clientcmd.BuildConfigFromFlags("", "/etc/petition/config")
	cloudCli, _ := kubernetes.NewForConfig(config)
	return *cloudCli
}

func addLabelAndSelector(pod *v1.Pod) {
	if pod.ObjectMeta.Labels == nil {
		pod.ObjectMeta.Labels = make(map[string]string)
	}
	pod.Labels["from-edge"] = "petition"
	if pod.Spec.NodeSelector == nil {
		pod.Spec.NodeSelector = make(map[string]string)
	}
	pod.Spec.NodeSelector["node-role.kubernetes.io/edge"] = ""

}
