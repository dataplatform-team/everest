// everest
// Copyright (C) 2023 Percona LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package api ...
package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/AlekSi/pointer"
	"github.com/labstack/echo/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	everestv1alpha1 "github.com/percona/everest-operator/api/v1alpha1"
	"github.com/percona/everest/pkg/rbac"
)

const (
	databaseClusterRestoreKind = "databaseclusterrestores"
)

// ListDatabaseClusterRestores List of the created database cluster restores on the specified kubernetes cluster.
func (e *EverestServer) ListDatabaseClusterRestores(ctx echo.Context, namespace, name string) error {
	req := ctx.Request()
	if err := validateRFC1035(name, "name"); err != nil {
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString(err.Error())})
	}
	val := url.Values{}
	val.Add("labelSelector", fmt.Sprintf("clusterName=%s", name))
	req.URL.RawQuery = val.Encode()
	path := req.URL.Path
	// trim restores
	path = strings.TrimSuffix(path, "/restores")
	// trim name
	path = strings.TrimSuffix(path, name)
	path = strings.ReplaceAll(path, "database-clusters", "database-cluster-restores")
	req.URL.Path = path

	user, err := rbac.GetUser(ctx)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to get user from context" + err.Error()),
		})
	}
	rbacFilter := transformK8sList(func(l *unstructured.UnstructuredList) error {
		allowed := []unstructured.Unstructured{}
		for _, obj := range l.Items {
			restore := &everestv1alpha1.DatabaseClusterRestore{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, restore); err != nil {
				e.l.Error(errors.Join(err, errors.New("failed to convert unstructured to DatabaseClusterRestore")))
				return err
			}
			if err := e.enforceDBClusterListRestoreRBAC(user, restore, rbac.ActionRead); errors.Is(err, errInsufficientPermissions) {
				continue
			} else if err != nil {
				return err
			}
			allowed = append(allowed, obj)
		}
		l.Items = allowed
		return nil
	})

	return e.proxyKubernetes(ctx, namespace, databaseClusterRestoreKind, "", rbacFilter)
}

// CreateDatabaseClusterRestore Create a database cluster restore on the specified kubernetes cluster.
func (e *EverestServer) CreateDatabaseClusterRestore(ctx echo.Context, namespace string) error {
	user, err := rbac.GetUser(ctx)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to get user from context: " + err.Error()),
		})
	}

	restore := &DatabaseClusterRestore{}
	if err := e.getBodyFromContext(ctx, restore); err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString("Could not get DatabaseClusterRestore from the request body"),
		})
	}

	if err := validateDatabaseClusterRestore(ctx.Request().Context(), namespace, restore, e.kubeClient); err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString(err.Error()),
		})
	}

	dbCluster, err := e.kubeClient.GetDatabaseCluster(ctx.Request().Context(), namespace, restore.Spec.DbClusterName)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString(err.Error()),
		})
	}

	srcBkp := pointer.Get(pointer.Get(restore.Spec).DataSource.DbClusterBackupName)
	if err := e.enforceDBRestoreRBAC(user, namespace, srcBkp, dbCluster.GetName()); err != nil {
		return err
	}

	if dbCluster.Status.Status == everestv1alpha1.AppStateRestoring {
		e.l.Error("failed creating restore because another one is in progress")
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString("Another restore process for this DB cluster is currently in progress. Wait for its completion before initiating another."),
		})
	}

	return e.proxyKubernetes(ctx, namespace, databaseClusterRestoreKind, "")
}

func (e *EverestServer) enforceDBRestoreRBAC(user, namespace, srcBackupName, dbClusterName string) error {
	if err := e.enforce(user, rbac.ResourceDatabaseClusterCredentials, rbac.ActionRead, rbac.ObjectName(namespace, dbClusterName)); err != nil {
		return err
	}
	if err := e.enforce(user, rbac.ResourceDatabaseClusterBackups, rbac.ActionRead, rbac.ObjectName(namespace, srcBackupName)); err != nil {
		return err
	}

	if err := e.enforce(user, rbac.ResourceDatabaseClusterRestores, rbac.ActionRead, rbac.ObjectName(namespace, dbClusterName)); err != nil {
		return err
	}
	return nil
}

// DeleteDatabaseClusterRestore Delete the specified cluster restore on the specified kubernetes cluster.
func (e *EverestServer) DeleteDatabaseClusterRestore(ctx echo.Context, namespace, name string) error {
	user, err := rbac.GetUser(ctx)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to get user from context" + err.Error()),
		})
	}

	rs, err := e.kubeClient.GetDatabaseClusterRestore(ctx.Request().Context(), namespace, name)
	if err != nil {
		return err
	}

	if err = e.enforceDBClusterListRestoreRBAC(user, rs, rbac.ActionDelete); err != nil {
		return err
	}

	return e.proxyKubernetes(ctx, namespace, databaseClusterRestoreKind, name)
}

// GetDatabaseClusterRestore Returns the specified cluster restore on the specified kubernetes cluster.
func (e *EverestServer) GetDatabaseClusterRestore(ctx echo.Context, namespace, name string) error {
	user, err := rbac.GetUser(ctx)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to get user from context" + err.Error()),
		})
	}

	rs, err := e.kubeClient.GetDatabaseClusterRestore(ctx.Request().Context(), namespace, name)
	if err != nil {
		return err
	}
	if err = e.enforceDBClusterListRestoreRBAC(user, rs, rbac.ActionRead); err != nil {
		return err
	}

	attachK8sTypeMeta(rs)
	return ctx.JSON(http.StatusOK, rs)
}

// UpdateDatabaseClusterRestore Replace the specified cluster restore on the specified kubernetes cluster.
func (e *EverestServer) UpdateDatabaseClusterRestore(ctx echo.Context, namespace, name string) error {
	user, err := rbac.GetUser(ctx)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to get user from context" + err.Error()),
		})
	}

	rs, err := e.kubeClient.GetDatabaseClusterRestore(ctx.Request().Context(), namespace, name)
	if err != nil {
		return err
	}

	if err = e.enforceDBClusterListRestoreRBAC(user, rs, rbac.ActionUpdate); err != nil {
		return err
	}

	restore := &DatabaseClusterRestore{}
	if err := e.getBodyFromContext(ctx, restore); err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString("Could not get DatabaseClusterRestore from the request body"),
		})
	}
	if err := validateMetadata(restore.Metadata); err != nil {
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString(err.Error())})
	}
	if err := validateDatabaseClusterRestore(ctx.Request().Context(), namespace, restore, e.kubeClient); err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString(err.Error()),
		})
	}

	return e.proxyKubernetes(ctx, namespace, databaseClusterRestoreKind, name)
}

func (e *EverestServer) enforceDBClusterListRestoreRBAC(user string, restore *everestv1alpha1.DatabaseClusterRestore, action string) error {
	err := e.enforce(user, rbac.ResourceDatabaseClusterRestores, action, rbac.ObjectName(restore.GetNamespace(), restore.Spec.DBClusterName))
	if err != nil {
		if !errors.Is(err, errInsufficientPermissions) {
			e.l.Error(errors.Join(err, errors.New("failed to check restore permissions")))
		}
		return err
	}
	return nil
}
