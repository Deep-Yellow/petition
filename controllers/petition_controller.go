/*
Copyright 2023.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PetitionReconciler reconciles a Petition object
type PetitionReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *PetitionReconciler) CreateDeploymentAndService(ctx context.Context, cli client.Client, pod *v1.Pod) (ctrl.Result, error) {
	var err error
	var dp appsv1.Deployment
	var replica int32 = 1
	labels := map[string]string{"app": pod.Name}
	dp.Namespace = pod.Namespace
	dp.Name = pod.Name
	dp.Spec.Replicas = &replica
	dp.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: labels}
	dp.Spec.Template.Labels = labels
	dp.Spec.Template.Spec = *pod.Spec.DeepCopy()
	err = cli.Create(ctx, &dp)
	if err != nil {
		return ctrl.Result{}, err
	}

	// if pod has port exposed, create a service
	if len(pod.Spec.Containers[0].Ports) > 0 {
		var svc v1.Service
		svc.Name = pod.Name
		svc.Namespace = pod.Namespace
		svc.Spec.Selector = labels
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

//+kubebuilder:rbac:groups=fdse.cloudnative,resources=petitions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fdse.cloudnative,resources=petitions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=fdse.cloudnative,resources=petitions/finalizers,verbs=update
//+kubebuilder:rbac:groups=fdse.cloudnative,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fdse.cloudnative,resources=pods/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Petition object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
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
	cloudClient, er1 := client.New(config, client.Options{Scheme: scheme})
	if er1 != nil {
		r.Log.Error(er1, "error on getting cloud client from CloudConfig")
		return ctrl.Result{}, er1
	}

	var errList []error
	for i, n := 0, len(podList.Items); i < n; i++ {
		pod := podList.Items[i]
		for j, m := 0, len(pod.Status.Conditions); j < m; j++ {
			condition := pod.Status.Conditions[j]
			if condition.Type == v1.PodScheduled && condition.Reason == "Unschedulable" {
				// create deployment and service on cloud
				_, er2 := r.CreateDeploymentAndService(ctx, cloudClient, &pod)
				if er2 != nil {
					errList = append(errList, er2)
				} else {
					// delete pending pod
					_ = r.Delete(ctx, &pod)
				}
			}
		}
	}
	if len(errList) > 0 {
		return ctrl.Result{}, errList[0]
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PetitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
		Complete(r)
}
