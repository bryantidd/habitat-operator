// Copyright (c) 2017 Chef Software Inc. and/or applicable contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/typed/apps/v1beta1"
	"k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	crv1 "github.com/kinvolk/habitat-operator/pkg/habitat/apis/cr/v1"
)

type HabitatController struct {
	config Config
	logger log.Logger
}

type Config struct {
	AppsV1beta1Client *v1beta1.AppsV1beta1Client
	CoreV1Client      *v1.CoreV1Client
	HabitatClient     *rest.RESTClient
	Scheme            *runtime.Scheme
}

func New(config Config, logger log.Logger) (*HabitatController, error) {
	if config.AppsV1beta1Client == nil {
		return nil, errors.New("invalid controller config: no AppsV1beta1Client")
	}
	if config.CoreV1Client == nil {
		return nil, errors.New("invalid controller config: no CoreV1Client")
	}
	if config.HabitatClient == nil {
		return nil, errors.New("invalid controller config: no HabitatClient")
	}
	if config.Scheme == nil {
		return nil, errors.New("invalid controller config: no Schema")
	}
	if logger == nil {
		return nil, errors.New("invalid controller config: no logger")
	}

	hc := &HabitatController{
		config: config,
		logger: logger,
	}

	return hc, nil
}

// Run starts a Habitat resource controller
func (hc *HabitatController) Run(ctx context.Context) error {
	level.Info(hc.logger).Log("msg", "Watching Service Group objects")

	_, err := hc.watchCustomResources(ctx)
	if err != nil {
		level.Error(hc.logger).Log("msg", "Failed to register watch for ServiceGroup resource", "err", err)
		return err
	}

	// This channel is closed when the context is canceled or times out.
	<-ctx.Done()

	// Err() contains the error, if any.
	return ctx.Err()
}

func (hc *HabitatController) watchCustomResources(ctx context.Context) (cache.Controller, error) {
	source := cache.NewListWatchFromClient(
		hc.config.HabitatClient,
		crv1.ServiceGroupResourcePlural,
		apiv1.NamespaceAll,
		fields.Everything())

	_, k8sController := cache.NewInformer(
		source,

		// The object type.
		&crv1.ServiceGroup{},

		// resyncPeriod
		// Every resyncPeriod, all resources in the cache will retrigger events.
		// Set to 0 to disable the resync.
		1*time.Minute,

		// Your custom resource event handlers.
		cache.ResourceEventHandlerFuncs{
			AddFunc:    hc.onAdd,
			UpdateFunc: hc.onUpdate,
			DeleteFunc: hc.onDelete,
		})

	// The k8sController will start processing events from the API.
	go k8sController.Run(ctx.Done())

	return k8sController, nil
}

func (hc *HabitatController) onAdd(obj interface{}) {
	sg, ok := obj.(*crv1.ServiceGroup)
	if !ok {
		level.Error(hc.logger).Log("msg", "unknown event type")
		return
	}

	level.Debug(hc.logger).Log("function", "onAdd", "msg", sg.ObjectMeta.SelfLink)

	// Validate object.
	if err := validateCustomObject(*sg); err != nil {
		if vErr, ok := err.(validationError); ok {
			level.Error(hc.logger).Log("type", "validation error", "msg", err, "key", vErr.Key)
			return
		}

		level.Error(hc.logger).Log("msg", err)
		return
	}

	level.Debug(hc.logger).Log("msg", "validated object")

	// Create a deployment.

	// This value needs to be passed as a *int32, so we convert it, assign it to a
	// variable and afterwards pass a pointer to it.
	count := int32(sg.Spec.Count)

	deployment := &appsv1beta1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: sg.Name,
		},
		Spec: appsv1beta1.DeploymentSpec{
			Replicas: &count,
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"habitat": "true",
					},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:  "habitat-service",
							Image: sg.Spec.Image,
							VolumeMounts: []apiv1.VolumeMount{
								{
									Name:      "config",
									MountPath: "/kubernetes-configmap",
									ReadOnly:  true,
								},
							},
						},
					},
					// Define the volume for the ConfigMap.
					Volumes: []apiv1.Volume{
						{
							Name: "config",
							VolumeSource: apiv1.VolumeSource{
								ConfigMap: &apiv1.ConfigMapVolumeSource{
									LocalObjectReference: apiv1.LocalObjectReference{
										Name: configMapName(sg),
									},
									Items: []apiv1.KeyToPath{
										{
											Key:  "peer-watch-file",
											Path: "peer-watch-file",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	d, err := hc.config.AppsV1beta1Client.Deployments(apiv1.NamespaceDefault).Create(deployment)
	if err != nil {
		level.Error(hc.logger).Log("msg", err)
		return
	}

	level.Info(hc.logger).Log("msg", "created deployment", "name", d.GetObjectMeta().GetName())

	// Create the ConfigMap for the peer watch file.
	configMap := &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configMapName(sg),
			// Declare this ConfigMap to be owned by the Deployment, so that deleting
			// the Deployment deletes the ConfigMap.
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "extensions/v1beta1",
					Kind:       "Deployment",
					Name:       sg.Name,
					UID:        d.UID,
				},
			},
		},
		// Initially, the file will be empty. It will be populated by the
		// controller once the first pod has gotten an IP assigned to it.
		Data: map[string]string{
			"peer-watch-file": "",
		},
	}

	{
		_, err := hc.config.CoreV1Client.ConfigMaps(apiv1.NamespaceDefault).Create(configMap)
		if err != nil {
			level.Error(hc.logger).Log("msg", err)
			return
		}
	}

	level.Debug(hc.logger).Log("msg", "created ConfigMap with peer IP", "object", configMap.Data["peer-ip"])
}

func (hc *HabitatController) onUpdate(oldObj, newObj interface{}) {
	oldServiceGroup := oldObj.(*crv1.ServiceGroup)
	newServiceGroup := newObj.(*crv1.ServiceGroup)
	level.Info(hc.logger).Log("function", "onUpdate", "msg", fmt.Sprintf("oldObj: %s, newObj: %s", oldServiceGroup.ObjectMeta.SelfLink, newServiceGroup.ObjectMeta.SelfLink))
}

func (hc *HabitatController) onDelete(obj interface{}) {
	sg, ok := obj.(*crv1.ServiceGroup)
	if !ok {
		level.Error(hc.logger).Log("msg", "unknown event type")
		return
	}

	level.Debug(hc.logger).Log("function", "onDelete", "msg", sg.ObjectMeta.SelfLink)

	deploymentsClient := hc.config.AppsV1beta1Client.Deployments(sg.ObjectMeta.Namespace)
	deploymentName := sg.Name

	// With this policy, dependent resources will be deleted, but we don't wait
	// for that to happen.
	deletePolicy := metav1.DeletePropagationBackground
	deleteOptions := &metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	err := deploymentsClient.Delete(deploymentName, deleteOptions)
	if err != nil {
		level.Error(hc.logger).Log("msg", err)
		return
	}

	level.Info(hc.logger).Log("msg", "deleted deployment", "name", deploymentName)
}

func configMapName(sg *crv1.ServiceGroup) string {
	return fmt.Sprintf("%s-peer-file", sg.Name)
}
