// Copyright 2018 AccelByte Inc
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

package iam

import (
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/AccelByte/go-restful-plugins/v3/pkg/jaeger"
	"github.com/cenkalti/backoff"
	"github.com/opentracing/opentracing-go"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
)

const (
	resourceNamespace = "NAMESPACE"
	resourceUser      = "USER"
	DefaultUserRoleID = "2251438839e948d783ec0e5281daf05b"
)

func (client *DefaultClient) permissionAllowed(grantedPermissions []Permission, requiredPermission Permission) bool {
	for _, grantedPermission := range grantedPermissions {
		grantedAction := grantedPermission.Action
		if grantedPermission.IsScheduled() {
			grantedAction = grantedPermission.ScheduledAction
		}

		if client.resourceAllowed(grantedPermission.Resource, requiredPermission.Resource) &&
			client.actionAllowed(grantedAction, requiredPermission.Action) {
			return true
		}
	}

	return false
}

func (client *DefaultClient) applyUserPermissionResourceValues(
	grantedPermissions []Permission, claims *JWTClaims, allowedNamespace string) []Permission {
	if allowedNamespace == "" {
		allowedNamespace = claims.Namespace
	}

	for i := range grantedPermissions {
		grantedPermissions[i].Resource = strings.ReplaceAll(
			grantedPermissions[i].Resource, "{userId}", claims.Subject)
		grantedPermissions[i].Resource = strings.ReplaceAll(
			grantedPermissions[i].Resource, "{namespace}", allowedNamespace)
	}

	return grantedPermissions
}

func (client *DefaultClient) resourceAllowed(accessPermissionResource string, requiredPermissionResource string) bool {
	requiredPermResSections := strings.Split(requiredPermissionResource, ":")
	requiredPermResSectionLen := len(requiredPermResSections)
	accessPermResSections := strings.Split(accessPermissionResource, ":")
	accessPermResSectionLen := len(accessPermResSections)

	minSectionLen := accessPermResSectionLen
	if minSectionLen > requiredPermResSectionLen {
		minSectionLen = requiredPermResSectionLen
	}

	for i := 0; i < minSectionLen; i++ {
		userSection := accessPermResSections[i]
		requiredSection := requiredPermResSections[i]

		if userSection != requiredSection && userSection != "*" {
			if strings.HasSuffix(userSection, "-") && i > 0 {
				previousSeg := accessPermResSections[i-1]
				if previousSeg == resourceNamespace {
					// assigned namespace `{studio}-` will allow studio & its related games
					// it is new format game namespace
					if strings.Contains(requiredSection, "-") && len(strings.Split(requiredSection, "-")) == 2 {
						if strings.HasPrefix(requiredSection, userSection) {
							continue
						}
						return false
					}
					// the request resource namespace is this studio
					if userSection == (requiredSection + "-") {
						continue
					}
					namespaceContextCache, err := client.namespaceContextCache.Get(requiredSection)
					if err != nil {
						logrus.Warnf("unable to get namespace context: %v", err)
						return false
					}
					namespaceContext := namespaceContextCache.(*NamespaceContext)
					if namespaceContext.NotFound {
						return false
					}
					if namespaceContext.Type == NamespaceTypeGame && userSection == (namespaceContext.StudioNamespace+"-") {
						continue
					}
				}
			}
			return false
		}
	}

	if accessPermResSectionLen == requiredPermResSectionLen {
		return true
	}

	if accessPermResSectionLen < requiredPermResSectionLen {
		if accessPermResSections[accessPermResSectionLen-1] == "*" {
			if accessPermResSectionLen < 2 {
				return true
			}

			segment := accessPermResSections[accessPermResSectionLen-2]
			if segment == resourceNamespace || segment == resourceUser {
				return false
			}

			return true
		}

		return false
	}

	for i := requiredPermResSectionLen; i < accessPermResSectionLen; i++ {
		if accessPermResSections[i] != "*" {
			return false
		}
	}

	return true
}

func (client *DefaultClient) actionAllowed(grantedAction int, requiredAction int) bool {
	return grantedAction&requiredAction == requiredAction
}

// nolint: funlen, dupl
func (client *DefaultClient) getRolePermission(roleID string, rootSpan opentracing.Span) ([]Permission, error) {
	span := jaeger.StartChildSpan(rootSpan, "client.getRolePermission")
	defer jaeger.Finish(span)

	if cachedRolePermission, found := client.rolePermissionCache.Get(roleID); found {
		rolePermissions := make([]Permission, len(cachedRolePermission.([]Permission)))
		_ = copy(rolePermissions, cachedRolePermission.([]Permission))

		return rolePermissions, nil
	}

	req, err := http.NewRequest("GET", client.config.BaseURL+getRolePath+"/"+roleID, nil)
	if err != nil {
		return nil, errors.Wrap(err, "getRolePermission: unable to create new HTTP request")
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+client.clientAccessToken.Load())

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = maxBackOffTime

	var responseStatusCode int

	var responseBodyBytes []byte

	err = backoff.
		Retry(
			func() error {
				reqSpan := jaeger.StartChildSpan(span, "HTTP Request: "+req.Method+" "+req.URL.Path)
				defer jaeger.Finish(reqSpan)
				jErr := jaeger.InjectSpanIntoRequest(reqSpan, req)
				logErr(jErr)

				resp, e := client.httpClient.Do(req)
				if e != nil {
					return backoff.Permanent(e)
				}
				defer resp.Body.Close()

				responseStatusCode = resp.StatusCode
				if resp.StatusCode >= http.StatusInternalServerError {
					jaeger.TraceError(reqSpan, fmt.Errorf("StatusCode: %v", resp.StatusCode))
					return errors.Errorf("getRolePermission: endpoint returned status code : %v", responseStatusCode)
				}

				responseBodyBytes, e = ioutil.ReadAll(resp.Body)
				if e != nil {
					jaeger.TraceError(reqSpan, fmt.Errorf("Body.ReadAll: %s", e))
					return errors.Wrap(e, "getRolePermission: unable to read response body")
				}

				return nil
			},
			b,
		)

	if err != nil {
		jaeger.TraceError(span, errors.Wrap(err, "getRolePermission: unable to do HTTP request"))
		return nil, errors.Wrap(err, "getRolePermission: unable to do HTTP request")
	}

	switch responseStatusCode {
	case http.StatusOK:
		// do nothing
	case http.StatusUnauthorized:
		jaeger.TraceError(span, errors.Wrap(errUnauthorized, "getRolePermission: unauthorized"))
		return nil, errors.Wrap(errUnauthorized, "getRolePermission: unauthorized")
	case http.StatusForbidden:
		jaeger.TraceError(span, errors.Wrap(errForbidden, "getRolePermission: forbidden"))
		return nil, errors.Wrap(errForbidden, "getRolePermission: forbidden")
	case http.StatusNotFound:
		jaeger.TraceError(span, errors.Wrap(errRoleNotFound, "getRolePermission: not found"))
		return nil, errors.Wrap(errRoleNotFound, "getRolePermission: not found")
	default:
		jaeger.TraceError(span, errors.New("unexpected error: "+http.StatusText(responseStatusCode)))
		return nil, errors.New("unexpected error: " + http.StatusText(responseStatusCode))
	}

	var role Role

	err = json.Unmarshal(responseBodyBytes, &role)
	if err != nil {
		jaeger.TraceError(span, errors.Wrap(err, "getRolePermission: unable to unmarshal response body"))
		return nil, errors.Wrap(err, "getRolePermission: unable to unmarshal response body")
	}

	rolePermissions := make([]Permission, len(role.Permissions))
	_ = copy(rolePermissions, role.Permissions)

	client.rolePermissionCache.Set(roleID, role.Permissions, cache.DefaultExpiration)

	return rolePermissions, nil
}

func buildRoleOverrideCacheKey(namespace, roleId string) string {
	return fmt.Sprintf("%s:%s", namespace, roleId)
}

func extractRoleOverrideCacheKey(cacheKey string) (namespace, roleId string, err error) {
	namespaceAndRoleId := strings.SplitN(cacheKey, ":", 2)
	if len(namespaceAndRoleId) != 2 {
		return "", "", fmt.Errorf("invalid cache key: %s", cacheKey)
	}
	return namespaceAndRoleId[0], namespaceAndRoleId[1], nil
}

// nolint: funlen, dupl
func (client *DefaultClient) remoteGetRoleNamespacePermission(namespace, roleID string, rootSpan opentracing.Span) ([]Permission, error) {
	span := jaeger.StartChildSpan(rootSpan, "client.remoteGetRoleNamespacePermission")
	defer jaeger.Finish(span)

	req, err := http.NewRequest("GET", fmt.Sprintf(client.config.BaseURL+getNamespaceRolePath, namespace, roleID), nil)
	if err != nil {
		return nil, errors.Wrap(err, "remoteGetRoleNamespacePermission: unable to create new HTTP request")
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+client.clientAccessToken.Load())

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = maxBackOffTime

	var responseStatusCode int

	var responseBodyBytes []byte

	err = backoff.
		Retry(
			func() error {
				reqSpan := jaeger.StartChildSpan(span, "HTTP Request: "+req.Method+" "+req.URL.Path)
				defer jaeger.Finish(reqSpan)
				jErr := jaeger.InjectSpanIntoRequest(reqSpan, req)
				logErr(jErr)

				resp, e := client.httpClient.Do(req)
				if e != nil {
					return backoff.Permanent(e)
				}
				defer resp.Body.Close()

				responseStatusCode = resp.StatusCode
				if resp.StatusCode >= http.StatusInternalServerError {
					jaeger.TraceError(reqSpan, fmt.Errorf("StatusCode: %v", resp.StatusCode))
					return errors.Errorf("remoteGetRoleNamespacePermission: endpoint returned status code : %v", responseStatusCode)
				}

				responseBodyBytes, e = ioutil.ReadAll(resp.Body)
				if e != nil {
					jaeger.TraceError(reqSpan, fmt.Errorf("Body.ReadAll: %s", e))
					return errors.Wrap(e, "remoteGetRoleNamespacePermission: unable to read response body")
				}

				return nil
			},
			b,
		)

	if err != nil {
		jaeger.TraceError(span, errors.Wrap(err, "remoteGetRoleNamespacePermission: unable to do HTTP request"))
		return nil, errors.Wrap(err, "remoteGetRoleNamespacePermission: unable to do HTTP request")
	}

	switch responseStatusCode {
	case http.StatusOK:
		// do nothing
	case http.StatusUnauthorized:
		jaeger.TraceError(span, errors.Wrap(errUnauthorized, "remoteGetRoleNamespacePermission: unauthorized"))
		return nil, errors.Wrap(errUnauthorized, "remoteGetRoleNamespacePermission: unauthorized")
	case http.StatusForbidden:
		jaeger.TraceError(span, errors.Wrap(errForbidden, "remoteGetRoleNamespacePermission: forbidden"))
		return nil, errors.Wrap(errForbidden, "remoteGetRoleNamespacePermission: forbidden")
	case http.StatusNotFound:
		jaeger.TraceError(span, errors.Wrap(errRoleNotFound, "remoteGetRoleNamespacePermission: not found"))
		return nil, errors.Wrap(errRoleNotFound, "remoteGetRoleNamespacePermission: not found")
	default:
		jaeger.TraceError(span, errors.New("unexpected error: "+http.StatusText(responseStatusCode)))
		return nil, errors.New("unexpected error: " + http.StatusText(responseStatusCode))
	}

	var namespacePermission RoleNamespacePermission

	err = json.Unmarshal(responseBodyBytes, &namespacePermission)
	if err != nil {
		jaeger.TraceError(span, errors.Wrap(err, "remoteGetRoleNamespacePermission: unable to unmarshal response body"))
		return nil, errors.Wrap(err, "remoteGetRoleNamespacePermission: unable to unmarshal response body")
	}

	rolePermissions := make([]Permission, len(namespacePermission.Permissions))
	_ = copy(rolePermissions, namespacePermission.Permissions)
	return rolePermissions, nil
}

// nolint: funlen, dupl
func (client *DefaultClient) GetRoleNamespacePermission(namespace, roleID, requestNamespace string, rootSpan opentracing.Span) ([]Permission, error) {
	span := jaeger.StartChildSpan(rootSpan, "client.GetRoleNamespacePermission")
	defer jaeger.Finish(span)

	if roleID == DefaultUserRoleID {
		queryNS := namespace
		if namespace == "*" || strings.HasSuffix(namespace, "-") {
			if requestNamespace != "" {
				queryNS = requestNamespace
			} else {
				goto skipRoleOverride
			}
		}
		permissions, err := client.roleNamespacePermissionCache.Get(buildRoleOverrideCacheKey(queryNS, roleID))
		if err != nil {
			return nil, errors.Wrap(err, "GetRoleNamespacePermission: unable to get role permissions")
		}
		rolePermissions := make([]Permission, len(permissions.([]Permission)))
		_ = copy(rolePermissions, permissions.([]Permission))

		return rolePermissions, nil
	}
skipRoleOverride:
	return client.getRolePermission(roleID, span)
}
