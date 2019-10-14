/*
Copyright 2019 The Crossplane Authors.

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

package database

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplaneio/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplaneio/crossplane-runtime/pkg/meta"
	"github.com/crossplaneio/crossplane-runtime/pkg/resource"
	"github.com/crossplaneio/crossplane-runtime/pkg/util"

	"github.com/crossplaneio/stack-gcp/apis/database/v1alpha2"
	apisv1alpha2 "github.com/crossplaneio/stack-gcp/apis/v1alpha2"
	gcp "github.com/crossplaneio/stack-gcp/pkg/clients"
	"github.com/crossplaneio/stack-gcp/pkg/clients/cloudsql"
)

const (
	errNotCloudsql                = "managed resource is not a CloudsqlInstance CR"
	errProviderNotRetrieved       = "provider could not be retrieved"
	errProviderSecretNotRetrieved = "secret referred in provider could not be retrieved"
	errManagedUpdateFailed        = "cannot update CloudsqlInstance CR"
	errConnectionNotRetrieved     = "cannot get connection details"

	errNewClient        = "cannot create new Sqladmin Service"
	errInsertFailed     = "cannot insert new Cloudsql instance"
	errDeleteFailed     = "cannot delete the Cloudsql instance"
	errPatchFailed      = "cannot patch the Cloudsql instance"
	errGetFailed        = "cannot get the Cloudsql instance"
	errUpdateRootFailed = "cannot update root user credentials"
)

// CloudsqlInstanceController is the controller for Cloudsql CRD.
type CloudsqlInstanceController struct{}

// SetupWithManager creates a new Controller and adds it to the Manager with default RBAC. The Manager will set fields
// on the Controller and Start it when the Manager is Started.
func (c *CloudsqlInstanceController) SetupWithManager(mgr ctrl.Manager) error {
	r := resource.NewManagedReconciler(mgr,
		resource.ManagedKind(v1alpha2.CloudsqlInstanceGroupVersionKind),
		resource.WithExternalConnecter(&cloudsqlConnector{kube: mgr.GetClient()}))

	name := strings.ToLower(fmt.Sprintf("%s.%s", v1alpha2.CloudsqlInstanceKindAPIVersion, v1alpha2.Group))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&v1alpha2.CloudsqlInstance{}).
		Complete(r)
}

type cloudsqlConnector struct {
	kube         client.Client
	newServiceFn func(ctx context.Context, opts ...option.ClientOption) (*sqladmin.Service, error)
}

func (c *cloudsqlConnector) Connect(ctx context.Context, mg resource.Managed) (resource.ExternalClient, error) {
	cr, ok := mg.(*v1alpha2.CloudsqlInstance)
	if !ok {
		return nil, errors.New(errNotCloudsql)
	}

	provider := &apisv1alpha2.Provider{}
	n := meta.NamespacedNameOf(cr.Spec.ProviderReference)
	if err := c.kube.Get(ctx, n, provider); err != nil {
		return nil, errors.Wrap(err, errProviderNotRetrieved)
	}
	secret := &v1.Secret{}
	name := meta.NamespacedNameOf(&v1.ObjectReference{
		Name:      provider.Spec.Secret.Name,
		Namespace: provider.Namespace,
	})
	if err := c.kube.Get(ctx, name, secret); err != nil {
		return nil, errors.Wrap(err, errProviderSecretNotRetrieved)
	}

	if c.newServiceFn == nil {
		c.newServiceFn = sqladmin.NewService
	}
	s, err := c.newServiceFn(ctx,
		option.WithCredentialsJSON(secret.Data[provider.Spec.Secret.Key]),
		option.WithScopes(sqladmin.SqlserviceAdminScope))
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &cloudsqlExternal{kube: c.kube, db: s.Instances, user: s.Users, projectID: provider.Spec.ProjectID}, nil
}

type cloudsqlExternal struct {
	kube      client.Client
	db        *sqladmin.InstancesService
	user      *sqladmin.UsersService
	projectID string
}

func (c *cloudsqlExternal) Observe(ctx context.Context, mg resource.Managed) (resource.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha2.CloudsqlInstance)
	if !ok {
		return resource.ExternalObservation{}, errors.New(errNotCloudsql)
	}
	instance, err := c.db.Get(c.projectID, meta.GetExternalName(cr)).Context(ctx).Do()
	if err != nil {
		return resource.ExternalObservation{}, errors.Wrap(resource.Ignore(gcp.IsErrorNotFound, err), errGetFailed)
	}
	cr.Status.AtProvider = cloudsql.GenerateObservation(*instance)
	currentSpec := cr.Spec.ForProvider.DeepCopy()
	cloudsql.LateInitializeSpec(&cr.Spec.ForProvider, *instance)
	// TODO(muvaf): reflection in production code might cause performance bottlenecks. Generating comparison
	// methods would make more sense.
	upToDate := reflect.DeepEqual(currentSpec, &cr.Spec.ForProvider)
	// TODO(muvaf): Should we always update to correct root password drifts via Update calls?
	if !upToDate {
		if err := c.kube.Update(ctx, cr); err != nil {
			return resource.ExternalObservation{}, errors.Wrap(err, errManagedUpdateFailed)
		}
	}
	var conn resource.ConnectionDetails
	switch cr.Status.AtProvider.State {
	case v1alpha2.StateRunnable:
		cr.Status.SetConditions(v1alpha1.Available())
		if !resource.IsBound(cr) {
			resource.SetBindable(cr)
		}
		conn, err = c.getConnectionDetails(ctx, cr)
		if err != nil {
			return resource.ExternalObservation{}, errors.Wrap(err, errConnectionNotRetrieved)
		}
	case v1alpha2.StateCreating:
		cr.Status.SetConditions(v1alpha1.Creating())
	case v1alpha2.StateCreationFailed, v1alpha2.StateSuspended, v1alpha2.StateMaintenance, v1alpha2.StateUnknownState:
		cr.Status.SetConditions(v1alpha1.Unavailable())
	}
	return resource.ExternalObservation{
		ResourceExists:    true,
		ResourceUpToDate:  upToDate,
		ConnectionDetails: conn,
	}, nil
}

func (c *cloudsqlExternal) Create(ctx context.Context, mg resource.Managed) (resource.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha2.CloudsqlInstance)
	if !ok {
		return resource.ExternalCreation{}, errors.New(errNotCloudsql)
	}
	instance := cloudsql.GenerateDatabaseInstance(cr.Spec.ForProvider, meta.GetExternalName(cr))
	_, err := c.db.Insert(c.projectID, instance).Context(ctx).Do()
	return resource.ExternalCreation{}, errors.Wrap(resource.Ignore(gcp.IsErrorAlreadyExists, err), errInsertFailed)
}

func (c *cloudsqlExternal) Update(ctx context.Context, mg resource.Managed) (resource.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha2.CloudsqlInstance)
	if !ok {
		return resource.ExternalUpdate{}, errors.New(errNotCloudsql)
	}
	conn, err := c.updateRootCredentials(ctx, cr)
	if err != nil {
		return resource.ExternalUpdate{}, errors.Wrap(err, errUpdateRootFailed)
	}
	instance := cloudsql.GenerateDatabaseInstance(cr.Spec.ForProvider, meta.GetExternalName(cr))
	_, err = c.db.Patch(c.projectID, meta.GetExternalName(cr), instance).Context(ctx).Do()
	return resource.ExternalUpdate{ConnectionDetails: conn}, errors.Wrap(err, errPatchFailed)
}

func (c *cloudsqlExternal) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha2.CloudsqlInstance)
	if !ok {
		return errors.New(errNotCloudsql)
	}
	_, err := c.db.Delete(c.projectID, meta.GetExternalName(cr)).Context(ctx).Do()
	if gcp.IsErrorNotFound(err) {
		return nil
	}
	return errors.Wrap(err, errDeleteFailed)
}

func (c *cloudsqlExternal) getConnectionDetails(ctx context.Context, cr *v1alpha2.CloudsqlInstance) (resource.ConnectionDetails, error) {
	m := map[string][]byte{
		v1alpha1.ResourceCredentialsSecretUserKey: []byte(cr.DatabaseUserName()),
	}
	s := &v1.Secret{}
	name := types.NamespacedName{Name: cr.Spec.WriteConnectionSecretToReference.Name, Namespace: cr.Namespace}
	err := c.kube.Get(ctx, name, s)
	if resource.IgnoreNotFound(err) != nil {
		return nil, err
	}
	if len(s.Data[v1alpha1.ResourceCredentialsSecretPasswordKey]) != 0 {
		m[v1alpha1.ResourceCredentialsSecretPasswordKey] = s.Data[v1alpha1.ResourceCredentialsSecretPasswordKey]
	}
	// TODO(muvaf): There might be cases where more than 1 private and/or public IP address has been assigned. We should
	// somehow show all addresses that are possible to use.
	for _, ip := range cr.Status.AtProvider.IPAddresses {
		if ip.Type == v1alpha2.PrivateIPType {
			m[v1alpha2.PrivateIPKey] = []byte(ip.IPAddress)
			// TODO(muvaf): we explicitly enforce use of private IP if it's available. But this should be configured
			// by resource class or claim.
			m[v1alpha1.ResourceCredentialsSecretEndpointKey] = []byte(ip.IPAddress)
		}
		if ip.Type == v1alpha2.PublicIPType {
			m[v1alpha2.PublicIPKey] = []byte(ip.IPAddress)
			if len(m[v1alpha1.ResourceCredentialsSecretEndpointKey]) == 0 {
				m[v1alpha1.ResourceCredentialsSecretEndpointKey] = []byte(ip.IPAddress)
			}
		}
	}
	return m, nil
}

func (c *cloudsqlExternal) updateRootCredentials(ctx context.Context, cr *v1alpha2.CloudsqlInstance) (resource.ConnectionDetails, error) {
	users, err := c.user.List(c.projectID, meta.GetExternalName(cr)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var rootUser *sqladmin.User
	for _, val := range users.Items {
		if val.Name == cr.DatabaseUserName() {
			rootUser = val
			break
		}
	}
	if rootUser == nil {
		return nil, &googleapi.Error{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("user: %s is not found", cr.DatabaseUserName()),
		}
	}
	conn, err := c.getConnectionDetails(ctx, cr)
	if err != nil {
		return nil, err
	}
	password := string(conn[v1alpha1.ResourceCredentialsSecretPasswordKey])
	if len(password) == 0 {
		password, err = util.GeneratePassword(v1alpha2.PasswordLength)
		if err != nil {
			return nil, err
		}
		conn[v1alpha1.ResourceCredentialsSecretPasswordKey] = []byte(password)
	}
	rootUser.Password = password
	_, err = c.user.Update(c.projectID, meta.GetExternalName(cr), rootUser.Name, rootUser).
		Host(rootUser.Host).
		Context(ctx).
		Do()
	return conn, err
}