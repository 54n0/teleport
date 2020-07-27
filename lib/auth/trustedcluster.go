/*
Copyright 2017 Gravitational, Inc.

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

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/httplib"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/roundtrip"
	"github.com/gravitational/trace"
)

// UpsertTrustedCluster creates or toggles a Trusted Cluster relationship.
func (a *AuthServer) UpsertTrustedCluster(ctx context.Context, trustedCluster services.TrustedCluster) (services.TrustedCluster, error) {
	var exists bool

	// It is recommended to omit trusted cluster name because the trusted cluster name
	// is updated to the roots cluster name during the handshake with the root cluster.
	var existingCluster services.TrustedCluster
	if trustedCluster.GetName() != "" {
		var err error
		if existingCluster, err = a.Presence.GetTrustedCluster(trustedCluster.GetName()); err == nil {
			exists = true
		}
	}

	enable := trustedCluster.GetEnabled()

	// If the trusted cluster already exists in the backend, make sure it's a
	// valid state change client is trying to make.
	if exists {
		if err := existingCluster.CanChangeStateTo(trustedCluster); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	// change state
	switch {
	case exists == true && enable == true:
		log.Debugf("Enabling existing Trusted Cluster relationship.")

		if err := a.activateCertAuthority(trustedCluster); err != nil {
			if trace.IsNotFound(err) {
				return nil, trace.BadParameter("enable only supported for Trusted Clusters created with Teleport 2.3 and above")
			}
			return nil, trace.Wrap(err)
		}

		if err := a.createReverseTunnel(trustedCluster); err != nil {
			return nil, trace.Wrap(err)
		}
	case exists == true && enable == false:
		log.Debugf("Disabling existing Trusted Cluster relationship.")

		if err := a.deactivateCertAuthority(trustedCluster); err != nil {
			if trace.IsNotFound(err) {
				return nil, trace.BadParameter("enable only supported for Trusted Clusters created with Teleport 2.3 and above")
			}
			return nil, trace.Wrap(err)
		}

		if err := a.DeleteReverseTunnel(trustedCluster.GetName()); err != nil {
			return nil, trace.Wrap(err)
		}
	case exists == false && enable == true:
		log.Debugf("Creating enabled Trusted Cluster relationship.")

		if err := a.checkLocalRoles(trustedCluster.GetRoleMap()); err != nil {
			return nil, trace.Wrap(err)
		}

		remoteCAs, err := a.establishTrust(trustedCluster)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		// Force name of the trusted cluster resource
		// to be equal to the name of the remote cluster it is connecting to.
		trustedCluster.SetName(remoteCAs[0].GetClusterName())

		if err := a.addCertAuthorities(trustedCluster, remoteCAs); err != nil {
			return nil, trace.Wrap(err)
		}

		if err := a.createReverseTunnel(trustedCluster); err != nil {
			return nil, trace.Wrap(err)
		}

	case exists == false && enable == false:
		log.Debugf("Creating disabled Trusted Cluster relationship.")

		if err := a.checkLocalRoles(trustedCluster.GetRoleMap()); err != nil {
			return nil, trace.Wrap(err)
		}

		remoteCAs, err := a.establishTrust(trustedCluster)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		// Force name to the name of the trusted cluster.
		trustedCluster.SetName(remoteCAs[0].GetClusterName())

		if err := a.addCertAuthorities(trustedCluster, remoteCAs); err != nil {
			return nil, trace.Wrap(err)
		}

		if err := a.deactivateCertAuthority(trustedCluster); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	tc, err := a.Presence.UpsertTrustedCluster(ctx, trustedCluster)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := a.EmitAuditEvent(events.TrustedClusterCreate, events.EventFields{
		events.EventUser: clientUsername(ctx),
	}); err != nil {
		log.Warnf("Failed to emit trusted cluster create event: %v", err)
	}

	return tc, nil
}

// EnsureTrustedClusters attempts to ensure that all currently registered
// trusted clusters are correctly configured.
//
// Trusted clusters are loaded if not supplied.
//
// This method may be called with a subset of the total trusted clusters
// present in the backend, but should not be called with a trusted cluster
// that does not already exist in the backend or which was not freshly loaded.
//
// This method is only public in order to facilitate certain integration tests.
func (a *AuthServer) EnsureTrustedClusters(ctx context.Context, tcs ...services.TrustedCluster) error {
	var err error
	if len(tcs) == 0 {
		tcs, err = a.GetTrustedClusters()
		if err != nil {
			return trace.Wrap(err)
		}
	}
	var errs []error
	for _, tc := range tcs {
		name := tc.GetName()
		renamed := false
		if tc.GetEnabled() {
			renamed, err = a.ensureEnabled(tc)
		} else {
			err = a.ensureDisabled(tc)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if !renamed {
			continue
		}
		// if the trusted cluster was renamed, first store it, then delete
		// the entry under the old name.
		if _, err = a.Presence.UpsertTrustedCluster(ctx, tc); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := a.Presence.DeleteTrustedCluster(ctx, name); err != nil {
			errs = append(errs, err)
			continue
		}
	}
	return trace.NewAggregate(errs...)
}

// ensureEnabled ensures that the supplied trusted cluster has its
// associated state enabled.  This function will automatically establish
// trust if trust was not previously established.  If establishing trust
// for the first time, the TrustedCluster resource may be renamed to match
// the true cluster name.
func (a *AuthServer) ensureEnabled(tc services.TrustedCluster) (renamed bool, err error) {
	err = a.activateCertAuthority(tc)
	if err != nil && !trace.IsNotFound(err) {
		return renamed, trace.Wrap(err)
	}
	if trace.IsNotFound(err) {
		// if we could not find CAs to activate, they are either already activated,
		// or trust has not yet been established.
		cas, err := a.getCertAuthorities(tc)
		if err != nil && !trace.IsNotFound(err) {
			return renamed, trace.Wrap(err)
		}
		if trace.IsNotFound(err) {
			// no active or inactive CAs, trust has not been established
			cas, err = a.establishTrust(tc)
			if err != nil {
				return renamed, trace.Wrap(err)
			}
			if name := cas[0].GetClusterName(); name != tc.GetName() {
				tc.SetName(name)
				renamed = true
			}
			if err := a.addCertAuthorities(tc, cas); err != nil {
				return renamed, trace.Wrap(err)
			}
		}
	}
	return renamed, trace.Wrap(a.createReverseTunnel(tc))
}

/*
func (a *AuthServer) ensureTrustEnabled(tc services.TrustedCluster) (renamed bool, err error) {
	if _, err := a.getCertAuthorities(tc); !trace.IsNotFound(err) {
		// either the CAs aleardy exist, or an unrelated error occurred.
		return renamed, trace.Wrap(err)
	}

	if err := a.activateCertAuthority(tc); !trace.IsNotFound(err) {
		// either the CAs were successfully enabled, or an unrelated error occurred.
		return renamed, trace.Wrap(err)
	}

	// if we get to this point, no CAs existed in either an active or
	// inactive state.  trust is being established for the first time.
	cas, err = a.establishTrust(tc)
	if err != nil {
		return renamed, trace.Wrap(err)
	}
	if name := cas[0].GetClusterName(); name != tc.GetName() {
		tc.SetName(name)
		renamed = true
	}
	if err := a.addCertAuthorities(tc, cas); err != nil {
		return renamed, trace.Wrap(err)
	}

	return renamed, nil
}*/

// ensureDisabled ensures that the supplied trusted cluster has had its associated
// state disabled.  This function does not differentiate between associated state
// which is already disabled, and assocaited state which does not exist.
func (a *AuthServer) ensureDisabled(tc services.TrustedCluster) error {
	if err := a.deactivateCertAuthority(tc); err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	if err := a.DeleteReverseTunnel(tc.GetName()); err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	return nil
}

func (a *AuthServer) checkLocalRoles(roleMap services.RoleMap) error {
	for _, mapping := range roleMap {
		for _, localRole := range mapping.Local {
			// expansion means dynamic mapping is in place,
			// so local role is undefined
			if utils.ContainsExpansion(localRole) {
				continue
			}
			_, err := a.GetRole(localRole)
			if err != nil {
				if trace.IsNotFound(err) {
					return trace.NotFound("a role %q referenced in a mapping %v:%v is not defined", localRole, mapping.Remote, mapping.Local)
				}
				return trace.Wrap(err)
			}
		}
	}
	return nil
}

// DeleteTrustedCluster removes services.CertAuthority, services.ReverseTunnel,
// and services.TrustedCluster resources.
func (a *AuthServer) DeleteTrustedCluster(ctx context.Context, name string) error {
	cn, err := a.GetClusterName()
	if err != nil {
		return trace.Wrap(err)
	}

	// This check ensures users are not deleting their root/own cluster.
	if cn.GetClusterName() == name {
		return trace.BadParameter("trusted cluster %q is the name of this root cluster and cannot be removed.", name)
	}

	if err := a.DeleteCertAuthority(services.CertAuthID{Type: services.HostCA, DomainName: name}); err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
	}

	if err := a.DeleteCertAuthority(services.CertAuthID{Type: services.UserCA, DomainName: name}); err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
	}

	if err := a.DeleteReverseTunnel(name); err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
	}

	if err := a.Presence.DeleteTrustedCluster(ctx, name); err != nil {
		return trace.Wrap(err)
	}

	if err := a.EmitAuditEvent(events.TrustedClusterDelete, events.EventFields{
		events.EventUser: clientUsername(ctx),
	}); err != nil {
		log.Warnf("Failed to emit trusted cluster delete event: %v", err)
	}

	return nil
}

func (a *AuthServer) establishTrust(trustedCluster services.TrustedCluster) ([]services.CertAuthority, error) {
	var localCertAuthorities []services.CertAuthority

	domainName, err := a.GetDomainName()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// get a list of certificate authorities for this auth server
	allLocalCAs, err := a.GetCertAuthorities(services.HostCA, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, lca := range allLocalCAs {
		if lca.GetClusterName() == domainName {
			localCertAuthorities = append(localCertAuthorities, lca)
		}
	}

	// create a request to validate a trusted cluster (token and local certificate authorities)
	validateRequest := ValidateTrustedClusterRequest{
		Token: trustedCluster.GetToken(),
		CAs:   localCertAuthorities,
	}

	// log the local certificate authorities that we are sending
	log.Debugf("Sending validate request; token=%v, CAs=%v", validateRequest.Token, validateRequest.CAs)

	// send the request to the remote auth server via the proxy
	validateResponse, err := a.sendValidateRequestToProxy(trustedCluster.GetProxyAddress(), &validateRequest)
	if err != nil {
		log.Error(err)
		if strings.Contains(err.Error(), "x509") {
			return nil, trace.AccessDenied("the trusted cluster uses misconfigured HTTP/TLS certificate.")
		}
		return nil, trace.Wrap(err)
	}

	// log the remote certificate authorities we are adding
	log.Debugf("Received validate response; CAs=%v", validateResponse.CAs)

	for _, ca := range validateResponse.CAs {
		for _, keyPair := range ca.GetTLSKeyPairs() {
			cert, err := tlsca.ParseCertificatePEM(keyPair.Cert)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			remoteClusterName, err := tlsca.ClusterName(cert.Subject)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if remoteClusterName == domainName {
				return nil, trace.BadParameter("remote cluster name can not be the same as local cluster name")
			}
			// TODO(klizhentas) in 2.5.0 prohibit adding trusted cluster resource name
			// different from cluster name (we had no way of checking this before x509,
			// because SSH CA was a public key, not a cert with metadata)
		}
	}

	return validateResponse.CAs, nil
}

func (a *AuthServer) addCertAuthorities(trustedCluster services.TrustedCluster, remoteCAs []services.CertAuthority) error {
	// the remote auth server has verified our token. add the
	// remote certificate authority to our backend
	for _, remoteCertAuthority := range remoteCAs {
		// change the name of the remote ca to the name of the trusted cluster
		remoteCertAuthority.SetName(trustedCluster.GetName())

		// wipe out roles sent from the remote cluster and set roles from the trusted cluster
		remoteCertAuthority.SetRoles(nil)
		if remoteCertAuthority.GetType() == services.UserCA {
			for _, r := range trustedCluster.GetRoles() {
				remoteCertAuthority.AddRole(r)
			}
			remoteCertAuthority.SetRoleMap(trustedCluster.GetRoleMap())
		}

		// we use create here instead of upsert to prevent people from wiping out
		// their own ca if it has the same name as the remote ca
		err := a.CreateCertAuthority(remoteCertAuthority)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	return nil
}

// DeleteRemoteCluster deletes remote cluster resource, all certificate authorities
// associated with it
func (a *AuthServer) DeleteRemoteCluster(clusterName string) error {
	// To make sure remote cluster exists - to protect against random
	// clusterName requests (e.g. when clusterName is set to local cluster name)
	_, err := a.Presence.GetRemoteCluster(clusterName)
	if err != nil {
		return trace.Wrap(err)
	}
	// delete cert authorities associated with the cluster
	err = a.DeleteCertAuthority(services.CertAuthID{
		Type:       services.HostCA,
		DomainName: clusterName,
	})
	if err != nil {
		// this method could have succeeded on the first call,
		// but then if the remote cluster resource could not be deleted
		// it would be impossible to delete the cluster after then
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
	}
	// there should be no User CA in trusted clusters on the main cluster side
	// per standard automation but clean up just in case
	err = a.DeleteCertAuthority(services.CertAuthID{
		Type:       services.UserCA,
		DomainName: clusterName,
	})
	if err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
	}
	return a.Presence.DeleteRemoteCluster(clusterName)
}

// GetRemoteCluster returns remote cluster by name
func (a *AuthServer) GetRemoteCluster(clusterName string) (services.RemoteCluster, error) {
	// To make sure remote cluster exists - to protect against random
	// clusterName requests (e.g. when clusterName is set to local cluster name)
	remoteCluster, err := a.Presence.GetRemoteCluster(clusterName)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.updateRemoteClusterStatus(remoteCluster); err != nil {
		return nil, trace.Wrap(err)
	}
	return remoteCluster, nil
}

func (a *AuthServer) updateRemoteClusterStatus(remoteCluster services.RemoteCluster) error {
	clusterConfig, err := a.GetClusterConfig()
	if err != nil {
		return trace.Wrap(err)
	}
	keepAliveCountMax := clusterConfig.GetKeepAliveCountMax()
	keepAliveInterval := clusterConfig.GetKeepAliveInterval()

	// fetch tunnel connections for the cluster to update runtime status
	connections, err := a.GetTunnelConnections(remoteCluster.GetName())
	if err != nil {
		return trace.Wrap(err)
	}
	remoteCluster.SetConnectionStatus(teleport.RemoteClusterStatusOffline)
	lastConn, err := services.LatestTunnelConnection(connections)
	if err == nil {
		offlineThreshold := time.Duration(keepAliveCountMax) * keepAliveInterval
		tunnelStatus := services.TunnelConnectionStatus(a.clock, lastConn, offlineThreshold)
		remoteCluster.SetConnectionStatus(tunnelStatus)
		remoteCluster.SetLastHeartbeat(lastConn.GetLastHeartbeat())
	}
	return nil
}

// GetRemoteClusters returns remote clusters with updated statuses
func (a *AuthServer) GetRemoteClusters(opts ...services.MarshalOption) ([]services.RemoteCluster, error) {
	// To make sure remote cluster exists - to protect against random
	// clusterName requests (e.g. when clusterName is set to local cluster name)
	remoteClusters, err := a.Presence.GetRemoteClusters(opts...)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for i := range remoteClusters {
		if err := a.updateRemoteClusterStatus(remoteClusters[i]); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return remoteClusters, nil
}

func (a *AuthServer) validateTrustedCluster(validateRequest *ValidateTrustedClusterRequest) (*ValidateTrustedClusterResponse, error) {
	domainName, err := a.GetDomainName()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// validate that we generated the token
	err = a.validateTrustedClusterToken(validateRequest.Token)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// log the remote certificate authorities we are adding
	log.Debugf("Received validate request: token=%v, CAs=%v", validateRequest.Token, validateRequest.CAs)

	// add remote cluster resource to keep track of the remote cluster
	var remoteClusterName string
	for _, certAuthority := range validateRequest.CAs {
		// don't add a ca with the same as as local cluster name
		if certAuthority.GetName() == domainName {
			return nil, trace.AccessDenied("remote certificate authority has same name as cluster certificate authority: %v", domainName)
		}
		remoteClusterName = certAuthority.GetName()
	}
	remoteCluster, err := services.NewRemoteCluster(remoteClusterName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = a.CreateRemoteCluster(remoteCluster)
	if err != nil {
		if !trace.IsAlreadyExists(err) {
			return nil, trace.Wrap(err)
		}
	}

	// token has been validated, upsert the given certificate authority
	for _, certAuthority := range validateRequest.CAs {
		err = a.UpsertCertAuthority(certAuthority)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	// export local cluster certificate authority and return it to the cluster
	validateResponse := ValidateTrustedClusterResponse{
		CAs: []services.CertAuthority{},
	}
	for _, caType := range []services.CertAuthType{services.HostCA, services.UserCA} {
		certAuthority, err := a.GetCertAuthority(
			services.CertAuthID{Type: caType, DomainName: domainName},
			false, services.SkipValidation())
		if err != nil {
			return nil, trace.Wrap(err)
		}
		validateResponse.CAs = append(validateResponse.CAs, certAuthority)
	}

	// log the local certificate authorities we are sending
	log.Debugf("Sending validate response: CAs=%v", validateResponse.CAs)

	return &validateResponse, nil
}

func (a *AuthServer) validateTrustedClusterToken(token string) error {
	roles, err := a.ValidateToken(token)
	if err != nil {
		return trace.AccessDenied("the remote server denied access: invalid cluster token")
	}

	if !roles.Include(teleport.RoleTrustedCluster) && !roles.Include(teleport.LegacyClusterTokenType) {
		return trace.AccessDenied("role does not match")
	}

	return nil
}

func (s *AuthServer) sendValidateRequestToProxy(host string, validateRequest *ValidateTrustedClusterRequest) (*ValidateTrustedClusterResponse, error) {
	proxyAddr := url.URL{
		Scheme: "https",
		Host:   host,
	}

	opts := []roundtrip.ClientParam{
		roundtrip.SanitizerEnabled(true),
	}

	if lib.IsInsecureDevMode() {
		log.Warn("The setting insecureSkipVerify is used to communicate with proxy. Make sure you intend to run Teleport in insecure mode!")

		// Get the default transport, this allows picking up proxy from the
		// environment.
		tr, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, trace.BadParameter("unable to get default transport")
		}

		// Disable certificate checking while in debug mode.
		tlsConfig := utils.TLSConfig(s.cipherSuites)
		tlsConfig.InsecureSkipVerify = true
		tr.TLSClientConfig = tlsConfig

		insecureWebClient := &http.Client{
			Transport: tr,
		}
		opts = append(opts, roundtrip.HTTPClient(insecureWebClient))
	}

	clt, err := roundtrip.NewClient(proxyAddr.String(), teleport.WebAPIVersion, opts...)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	validateRequestRaw, err := validateRequest.ToRaw()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	out, err := httplib.ConvertResponse(clt.PostJSON(context.TODO(), clt.Endpoint("webapi", "trustedclusters", "validate"), validateRequestRaw))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var validateResponseRaw *ValidateTrustedClusterResponseRaw
	err = json.Unmarshal(out.Bytes(), &validateResponseRaw)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	validateResponse, err := validateResponseRaw.ToNative()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return validateResponse, nil
}

type ValidateTrustedClusterRequest struct {
	Token string                   `json:"token"`
	CAs   []services.CertAuthority `json:"certificate_authorities"`
}

func (v *ValidateTrustedClusterRequest) ToRaw() (*ValidateTrustedClusterRequestRaw, error) {
	cas := [][]byte{}

	for _, certAuthority := range v.CAs {
		data, err := services.GetCertAuthorityMarshaler().MarshalCertAuthority(certAuthority)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		cas = append(cas, data)
	}

	return &ValidateTrustedClusterRequestRaw{
		Token: v.Token,
		CAs:   cas,
	}, nil
}

type ValidateTrustedClusterRequestRaw struct {
	Token string   `json:"token"`
	CAs   [][]byte `json:"certificate_authorities"`
}

func (v *ValidateTrustedClusterRequestRaw) ToNative() (*ValidateTrustedClusterRequest, error) {
	cas := []services.CertAuthority{}

	for _, rawCertAuthority := range v.CAs {
		certAuthority, err := services.GetCertAuthorityMarshaler().UnmarshalCertAuthority(rawCertAuthority)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		cas = append(cas, certAuthority)
	}

	return &ValidateTrustedClusterRequest{
		Token: v.Token,
		CAs:   cas,
	}, nil
}

type ValidateTrustedClusterResponse struct {
	CAs []services.CertAuthority `json:"certificate_authorities"`
}

func (v *ValidateTrustedClusterResponse) ToRaw() (*ValidateTrustedClusterResponseRaw, error) {
	cas := [][]byte{}

	for _, certAuthority := range v.CAs {
		data, err := services.GetCertAuthorityMarshaler().MarshalCertAuthority(certAuthority)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		cas = append(cas, data)
	}

	return &ValidateTrustedClusterResponseRaw{
		CAs: cas,
	}, nil
}

type ValidateTrustedClusterResponseRaw struct {
	CAs [][]byte `json:"certificate_authorities"`
}

func (v *ValidateTrustedClusterResponseRaw) ToNative() (*ValidateTrustedClusterResponse, error) {
	cas := []services.CertAuthority{}

	for _, rawCertAuthority := range v.CAs {
		certAuthority, err := services.GetCertAuthorityMarshaler().UnmarshalCertAuthority(rawCertAuthority)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		cas = append(cas, certAuthority)
	}

	return &ValidateTrustedClusterResponse{
		CAs: cas,
	}, nil
}

// activateCertAuthority will activate both the user and host certificate
// authority given in the services.TrustedCluster resource.
func (a *AuthServer) activateCertAuthority(t services.TrustedCluster) error {
	// TODO(fspmarshall): This function needs work.  We can currently get ourselves
	// into a bad state if an error occurs between the first and second activations.
	// ActivateCertAuthority should probably have a variant which does not fail if
	// the CA is already activated.
	_, err := a.ActivateCertAuthority(services.CertAuthID{Type: services.UserCA, DomainName: t.GetName()})
	if err != nil {
		return trace.Wrap(err)
	}

	_, err = a.ActivateCertAuthority(services.CertAuthID{Type: services.HostCA, DomainName: t.GetName()})
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// getCertAuthorities loads the user and host CAs associated with a trusted cluster.
func (a *AuthServer) getCertAuthorities(t services.TrustedCluster) ([]services.CertAuthority, error) {
	userCA, err := a.GetCertAuthority(services.CertAuthID{Type: services.UserCA, DomainName: t.GetName()}, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	hostCA, err := a.GetCertAuthority(services.CertAuthID{Type: services.HostCA, DomainName: t.GetName()}, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return []services.CertAuthority{
		userCA,
		hostCA,
	}, nil
}

// deactivateCertAuthority will deactivate both the user and host certificate
// authority given in the services.TrustedCluster resource.
func (a *AuthServer) deactivateCertAuthority(t services.TrustedCluster) error {
	err := a.DeactivateCertAuthority(services.CertAuthID{Type: services.UserCA, DomainName: t.GetName()})
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(a.DeactivateCertAuthority(services.CertAuthID{Type: services.HostCA, DomainName: t.GetName()}))
}

// createReverseTunnel will create a services.ReverseTunnel givenin the
// services.TrustedCluster resource.
func (a *AuthServer) createReverseTunnel(t services.TrustedCluster) error {
	reverseTunnel := services.NewReverseTunnel(
		t.GetName(),
		[]string{t.GetReverseTunnelAddress()},
	)
	return trace.Wrap(a.UpsertReverseTunnel(reverseTunnel))
}

// suspectedOrphanCA is a certificate authority which is suspected
// of being in an orphaned/dangline state - not associated with any
// valid trusted cluster resource.
type suspectedOrphanCA struct {
	ca    services.CertAuthority
	since time.Time
	seen  bool
}

// trustController manages state for periodic trusted
// cluster operations.
type trustController struct {
	// orphanAfter is the duration after which a CA is
	// considered orphaned and is removed from the backend.
	orphanAfter time.Duration
	// suspectedOrphanCAs are trusted CAs which don't appear to
	// belong either to this cluster.
	suspectedOrphanCAs []suspectedOrphanCA
}

func (c *trustController) cycle(ctx context.Context, auth *AuthServer, t time.Time) error {

	domainName, err := auth.GetDomainName()
	if err != nil {
		return trace.Wrap(err)
	}

	// get trusted clusters from the *backend*, not the cache.  This is very
	// important, since reading the trusted cluster list from a flaky cache
	// could cause us to erroneously prune CAs.
	tcs, err := auth.AuthServices.GetTrustedClusters()
	if err != nil {
		return trace.Wrap(err)
	}

	// first, attempt to ensure that all existant trusted clusters have
	// had their configurations correctly applied.
	if err := auth.EnsureTrustedClusters(ctx, tcs...); err != nil {
		// this is a best-effort operation, so just log the error
		// and keep working.
		log.Warnf("EnsureTrustedClusters failed: %v", err)
	}

	// reset seen tag for all existing suspects
	for _, sus := range c.suspectedOrphanCAs {
		sus.seen = false
	}

	var nextSuspects []suspectedOrphanCA

	for _, caType := range []services.CertAuthType{services.UserCA, services.HostCA} {
		cas, err := auth.GetCertAuthorities(caType, false)
		if err != nil {
			return trace.Wrap(err)
		}
		//
	Processing:
		for _, ca := range cas {
			if ca.GetClusterName() == domainName {
				// CA is associated with the local cluster - not orphaned.
				continue Processing
			}
			for _, tc := range tcs {
				if tc.GetName() == ca.GetClusterName() {
					// CA is associated with a trusted cluster - not orphaned.
					continue Processing
				}
			}
			// if we got this far, the ca *might* be in an orphaned/dangling state.
			for _, sus := range c.suspectedOrphanCAs {
				if ca.Equals(sus.ca) {
					// we are already tracking this suspect, mark it as seen
					// and continue processing.
					sus.seen = true
					continue Processing
				}
			}
			// we are not currently tracking this suspect, add it to the
			// upcoming suspect set.
			nextSuspects = append(nextSuspects, suspectedOrphanCA{
				ca:    ca,
				seen:  true,
				since: t,
			})
		}
	}

	for _, sus := range c.suspectedOrphanCAs {
		if !sus.seen {
			// suspect was either removed, or its associated trusted
			// cluster configuration is now present.
			continue
		}
		if t.After(sus.since) && t.Sub(sus.since) > c.orphanAfter {
			// orphan cutoff reached, attempt to remove CA
			if err := auth.DeleteCertAuthority(sus.ca.GetID()); err != nil && !trace.IsNotFound(err) {
				log.Warnf("Failed to remove orphan %s CA: %q", sus.ca.GetType(), sus.ca.GetName())
			}
			continue
		}
		// still a suspect, but not yet past the cutoff
		nextSuspects = append(nextSuspects, sus)
	}
	c.suspectedOrphanCAs = nextSuspects
	return nil
}
