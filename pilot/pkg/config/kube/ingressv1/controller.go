// Copyright Istio Authors
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

// Package ingress provides a read-only view of Kubernetes ingress resources
// as an ingress rule configuration type store
package ingress

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/hashicorp/go-multierror"
	knetworking "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ingressinformer "k8s.io/client-go/informers/networking/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/queue"
	"istio.io/pkg/env"
	"istio.io/pkg/log"
)

// In 1.0, the Gateway is defined in the namespace where the actual controller runs, and needs to be managed by
// user.
// The gateway is named by appending "-istio-autogenerated-k8s-ingress" to the name of the ingress.
//
// Currently the gateway namespace is hardcoded to istio-system (model.IstioIngressNamespace)
//
// VirtualServices are also auto-generated in the model.IstioIngressNamespace.
//
// The sync of Ingress objects to IP is done by status.go
// the 'ingress service' name is used to get the IP of the Service
// If ingress service is empty, it falls back to NodeExternalIP list, selected using the labels.
// This is using 'namespace' of pilot - but seems to be broken (never worked), since it uses Pilot's pod labels
// instead of the ingress labels.

// Follows mesh.IngressControllerMode setting to enable - OFF|STRICT|DEFAULT.
// STRICT requires "kubernetes.io/ingress.class" == mesh.IngressClass
// DEFAULT allows Ingress without explicit class.

// In 1.1:
// - K8S_INGRESS_NS - namespace of the Gateway that will act as ingress.
// - labels of the gateway set to "app=ingressgateway" for node_port, service set to 'ingressgateway' (matching default install)
//   If we need more flexibility - we can add it (but likely we'll deprecate ingress support first)
// -

var schemas = collection.SchemasFor(
	collections.IstioNetworkingV1Alpha3Virtualservices,
	collections.IstioNetworkingV1Alpha3Gateways)

// Control needs RBAC permissions to write to Pods.

type controller struct {
	meshWatcher  mesh.Holder
	domainSuffix string

	queue                  queue.Instance
	virtualServiceHandlers []func(config.Config, config.Config, model.Event)
	gatewayHandlers        []func(config.Config, config.Config, model.Event)

	ingressInformer cache.SharedInformer
	serviceInformer cache.SharedInformer
	serviceLister   listerv1.ServiceLister
	// May be nil if ingress class is not supported in the cluster
	classes ingressinformer.IngressClassInformer
}

// TODO: move to features ( and remove in 1.2 )
var ingressNamespace = env.RegisterStringVar("K8S_INGRESS_NS", "", "").Get()

var errUnsupportedOp = errors.New("unsupported operation: the ingress config store is a read-only view")

// NewController creates a new Kubernetes controller
func NewController(client kube.Client, meshWatcher mesh.Holder,
	options kubecontroller.Options) model.ConfigStoreCache {
	// queue requires a time duration for a retry delay after a handler error
	q := queue.NewQueue(1 * time.Second)

	if ingressNamespace == "" {
		ingressNamespace = constants.IstioIngressNamespace
	}

	ingressInformer := client.KubeInformer().Networking().V1().Ingresses().Informer()

	serviceInformer := client.KubeInformer().Core().V1().Services()

	classes := client.KubeInformer().Networking().V1().IngressClasses()
	classes.Informer()

	c := &controller{
		meshWatcher:     meshWatcher,
		domainSuffix:    options.DomainSuffix,
		queue:           q,
		ingressInformer: ingressInformer,
		classes:         classes,
		serviceInformer: serviceInformer.Informer(),
		serviceLister:   serviceInformer.Lister(),
	}

	ingressInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				q.Push(func() error {
					return c.onEvent(nil, obj, model.EventAdd)
				})
			},
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					q.Push(func() error {
						return c.onEvent(old, cur, model.EventUpdate)
					})
				}
			},
			DeleteFunc: func(obj interface{}) {
				q.Push(func() error {
					return c.onEvent(nil, obj, model.EventDelete)
				})
			},
		})

	return c
}

func (c *controller) shouldProcessIngress(mesh *meshconfig.MeshConfig, i *knetworking.Ingress) (bool, error) {
	var class *knetworking.IngressClass
	if c.classes != nil && i.Spec.IngressClassName != nil {
		c, err := c.classes.Lister().Get(*i.Spec.IngressClassName)
		if err != nil && !kerrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to get ingress class %v: %v", i.Spec.IngressClassName, err)
		}
		class = c
	}
	return shouldProcessIngressWithClass(mesh, i, class), nil
}

// shouldProcessIngressUpdate checks whether we should renotify registered handlers about an update event
func (c *controller) shouldProcessIngressUpdate(oldObj, curObj interface{}) (bool, error) {
	var shouldProcess bool

	// should always have curObj passed
	ing, ok := curObj.(*knetworking.Ingress)
	if !ok {
		return false, nil
	}

	if oldObj == nil { // corresponds to additions and deletions of ingresses, update handlers if the current version should be targeted
		shouldProcessUpdate, err := c.shouldProcessIngress(c.meshWatcher.Mesh(), ing)
		if err != nil {
			return false, err
		}
		shouldProcess = shouldProcessUpdate
	} else { // this case corresponds to an update to an existing ingress resource
		oldIng, ok := oldObj.(*knetworking.Ingress)
		if !ok {
			return false, nil
		}

		shouldProcessOld, err := c.shouldProcessIngress(c.meshWatcher.Mesh(), oldIng)
		if err != nil {
			return false, err
		}
		shouldProcessNew, err := c.shouldProcessIngress(c.meshWatcher.Mesh(), ing)
		if err != nil {
			return false, err
		}

		// the singular case we want to ignore is where neither the old nor new version of the ingress
		// should be targeted. otherwise we need to delete the ingress routes, add the ingress routes,
		// or change something about the ingress configuration
		shouldProcess = shouldProcessOld || shouldProcessNew
	}
	return shouldProcess, nil
}

func (c *controller) onEvent(oldObj, curObj interface{}, event model.Event) error {
	if !c.HasSynced() {
		return errors.New("waiting till full synchronization")
	}

	shouldProcess, err := c.shouldProcessIngressUpdate(oldObj, curObj)
	if err != nil {
		return err
	}
	if !shouldProcess {
		return nil
	}

	// Trigger updates for Gateway and VirtualService
	// TODO: we could be smarter here and only trigger when real changes were found
	for _, f := range c.virtualServiceHandlers {
		f(config.Config{}, config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.VirtualService,
			},
		}, event)
	}
	for _, f := range c.gatewayHandlers {
		f(config.Config{}, config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.Gateway,
			},
		}, event)
	}

	return nil
}

func (c *controller) RegisterEventHandler(kind config.GroupVersionKind, f func(config.Config, config.Config, model.Event)) {
	switch kind {
	case gvk.VirtualService:
		c.virtualServiceHandlers = append(c.virtualServiceHandlers, f)
	case gvk.Gateway:
		c.gatewayHandlers = append(c.gatewayHandlers, f)
	}
}

func (c *controller) SetWatchErrorHandler(handler func(r *cache.Reflector, err error)) error {
	var errs error
	if err := c.serviceInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(err, errs)
	}
	if err := c.ingressInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(err, errs)
	}
	return errs
}

func (c *controller) HasSynced() bool {
	return c.ingressInformer.HasSynced() && c.serviceInformer.HasSynced() &&
		(c.classes == nil || c.classes.Informer().HasSynced())
}

func (c *controller) Run(stop <-chan struct{}) {
	if !cache.WaitForCacheSync(stop, c.HasSynced) {
		log.Error("Failed to sync controller cache")
		return
	}
	c.queue.Run(stop)
	<-stop
}

func (c *controller) Schemas() collection.Schemas {
	// TODO: are these two config descriptors right?
	return schemas
}

func (c *controller) Get(typ config.GroupVersionKind, name, namespace string) *config.Config {
	return nil
}

// sortIngressByCreationTime sorts the list of config objects in ascending order by their creation time (if available).
func sortIngressByCreationTime(configs []interface{}) []*knetworking.Ingress {
	ingr := make([]*knetworking.Ingress, 0, len(configs))
	for _, i := range configs {
		ingr = append(ingr, i.(*knetworking.Ingress))
	}
	sort.SliceStable(ingr, func(i, j int) bool {
		// If creation time is the same, then behavior is nondeterministic. In this case, we can
		// pick an arbitrary but consistent ordering based on name and namespace, which is unique.
		// CreationTimestamp is stored in seconds, so this is not uncommon.
		if ingr[i].CreationTimestamp == ingr[j].CreationTimestamp {
			in := ingr[i].Name + "." + ingr[i].Namespace
			jn := ingr[j].Name + "." + ingr[j].Namespace
			return in < jn
		}
		return ingr[i].CreationTimestamp.Before(&ingr[j].CreationTimestamp)
	})
	return ingr
}

func (c *controller) List(typ config.GroupVersionKind, namespace string) ([]config.Config, error) {
	if typ != gvk.Gateway &&
		typ != gvk.VirtualService {
		return nil, errUnsupportedOp
	}

	out := make([]config.Config, 0)

	ingressByHost := map[string]*config.Config{}

	for _, ingress := range sortIngressByCreationTime(c.ingressInformer.GetStore().List()) {
		if namespace != "" && namespace != ingress.Namespace {
			continue
		}
		process, err := c.shouldProcessIngress(c.meshWatcher.Mesh(), ingress)
		if err != nil {
			return nil, err
		}
		if !process {
			continue
		}

		switch typ {
		case gvk.VirtualService:
			ConvertIngressVirtualService(*ingress, c.domainSuffix, ingressByHost, c.serviceLister)
		case gvk.Gateway:
			gateways := ConvertIngressV1alpha3(*ingress, c.meshWatcher.Mesh(), c.domainSuffix)
			out = append(out, gateways)
		}
	}

	if typ == gvk.VirtualService {
		for _, obj := range ingressByHost {
			out = append(out, *obj)
		}
	}

	return out, nil
}

func (c *controller) Create(_ config.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Update(_ config.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) UpdateStatus(config.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Patch(_ config.Config, _ config.PatchFunc) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Delete(_ config.GroupVersionKind, _, _ string, _ *string) error {
	return errUnsupportedOp
}