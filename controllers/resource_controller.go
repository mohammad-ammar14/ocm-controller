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
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	ociclient "github.com/fluxcd/pkg/oci/client"
	"github.com/google/go-containerregistry/pkg/name"
	gcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	v1alpha1 "github.com/open-component-model/ocm-controller/api/v1alpha1"
	ocmapi "github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc/versions/ocm.software/v3alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type contextKey string

// ResourceReconciler reconciles a Resource object
type ResourceReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	OCIRegistryAddr string
}

//+kubebuilder:rbac:groups=delivery.ocm.software,resources=resources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=delivery.ocm.software,resources=resources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=delivery.ocm.software,resources=resources/finalizers,verbs=update

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Resource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithName("resource-controller")

	log.Info("starting resource reconcile loop")
	resource := &v1alpha1.Resource{}
	if err := r.Client.Get(ctx, req.NamespacedName, resource); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get resource object: %w", err)
	}
	log.Info("found resource", "resource", resource)

	return r.reconcile(ctx, resource)
}

func (r *ResourceReconciler) reconcile(ctx context.Context, obj *v1alpha1.Resource) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithName("resource-controller")

	log.Info("finding component ref", "resource", obj)

	// get the component descriptor
	cdKey := types.NamespacedName{
		Name:      strings.ReplaceAll(obj.Spec.ComponentRef.Name, "/", "-"),
		Namespace: obj.Spec.ComponentRef.Namespace,
	}

	componentDescriptor := &v1alpha1.ComponentDescriptor{}
	if err := r.Get(ctx, cdKey, componentDescriptor); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(4).Info("component descriptor not found", "component", obj.Spec.ComponentRef)
			return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
		}

		return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()},
			fmt.Errorf("failed to get component object: %w", err)
	}

	log.Info("got component descriptor", "component descriptor", cdKey.String())

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(obj, r.Client)
	if err != nil {
		return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, err
	}

	// lookup the resource
	for _, res := range componentDescriptor.Spec.Resources {
		if res.Name != obj.Spec.Resource.Name {
			continue
		}

		// push the resource snapshot to oci
		snapshotName := fmt.Sprintf("%s/snapshots/%s:%s", r.OCIRegistryAddr, obj.Spec.SnapshotTemplate.Name, obj.Spec.SnapshotTemplate.Tag)
		if err := r.copyResourceToSnapshot(ctx, snapshotName, res); err != nil {
			return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, err
		}

		// create/update the snapshot custom resource
		snapshotCR := &v1alpha1.Snapshot{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: obj.GetNamespace(),
				Name:      obj.Spec.SnapshotTemplate.Name,
			},
		}

		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, snapshotCR, func() error {
			if snapshotCR.ObjectMeta.CreationTimestamp.IsZero() {
				controllerutil.SetOwnerReference(obj, snapshotCR, r.Scheme)
			}
			snapshotCR.Spec = v1alpha1.SnapshotSpec{
				Ref: strings.TrimPrefix(snapshotName, r.OCIRegistryAddr+"/snapshots/"),
			}
			return nil
		})

		if err != nil {
			return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()},
				fmt.Errorf("failed to create or update component descriptor: %w", err)
		}

		obj.Status.LastAppliedResourceVersion = res.Version

		log.Info("sucessfully created snapshot", "name", snapshotName)
	}

	obj.Status.ObservedGeneration = obj.GetGeneration()

	if err := patchHelper.Patch(ctx, obj); err != nil {
		return ctrl.Result{
			RequeueAfter: obj.GetRequeueAfter(),
		}, fmt.Errorf("failed to patch resource and set snaphost value: %w", err)
	}

	log.Info("sucessfully reconciled resource", "name", obj.GetName())

	return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
}

func (r *ResourceReconciler) copyResourceToSnapshot(ctx context.Context, snapshotName string, res ocmapi.Resource) error {
	ref := res.Access.Object["globalAccess"].(map[string]interface{})["ref"].(string)
	sha := res.Access.Object["globalAccess"].(map[string]interface{})["digest"].(string)
	digest, err := name.NewDigest(fmt.Sprintf("%s:%s@%s", ref, res.Version, sha), name.Insecure)
	if err != nil {
		return fmt.Errorf("failed to get component object: %w", err)
	}

	// proxy image requests via the in-cluster oci-registry
	proxyURL, err := url.Parse(fmt.Sprintf("http://%s", r.OCIRegistryAddr))
	if err != nil {
		return fmt.Errorf("failed to parse oci registry url: %w", err)
	}

	// create a transport to the in-cluster oci-registry
	tr := newCustomTransport(remote.DefaultTransport.(*http.Transport).Clone(), proxyURL)

	// set context values to be transmitted as headers on the registry requests
	for k, v := range map[string]string{
		"registry":   digest.Repository.Registry.String(),
		"repository": digest.Repository.String(),
		"digest":     digest.String(),
		"image":      digest.Name(),
		"tag":        res.Version,
	} {
		ctx = context.WithValue(ctx, contextKey(k), v)
	}

	// fetch the layer
	layer, err := remote.Layer(digest, remote.WithTransport(tr), remote.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to get component object: %w", err)
	}

	// create snapshot with single layer
	snapshot, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return fmt.Errorf("failed to get append layer: %w", err)
	}

	snapshotRef, err := name.ParseReference(snapshotName, name.Insecure)
	if err != nil {
		return fmt.Errorf("failed to create snapshot reference: %w", err)
	}

	ct := time.Now()
	snapshotMeta := ociclient.Metadata{
		Created: ct.Format(time.RFC3339),
		Digest:  snapshotRef.String(),
	}

	// add metadata
	snapshot = mutate.Annotations(snapshot, snapshotMeta.ToAnnotations()).(gcrv1.Image)

	// write snapshot to registry
	if err := remote.Write(snapshotRef, snapshot); err != nil {
		return fmt.Errorf("failed to get component object: %w", err)
	}

	return nil
}

type customTransport struct {
	http.RoundTripper
}

func newCustomTransport(upstream *http.Transport, proxyURL *url.URL) *customTransport {
	upstream.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	upstream.Proxy = http.ProxyURL(proxyURL)
	upstream.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	return &customTransport{upstream}
}

func (ct *customTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	keys := []string{"digest", "registry", "repository", "tag", "image"}
	for _, key := range keys {
		value := req.Context().Value(contextKey(key))
		if value != nil {
			req.Header.Set("x-"+key, value.(string))
		}
	}
	return ct.RoundTripper.RoundTrip(req)
}