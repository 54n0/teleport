/*
Copyright 2021 Gravitational, Inc.

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

package client

import (
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"

	"github.com/gravitational/teleport/api/constants"

	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/http2"
)

// Credentials are used to authenticate to Auth.
type Credentials interface {
	// Dialer is used to connect to Auth.
	Dialer() (ContextDialer, error)
	// TLSConfig returns TLS configuration used to connect to Auth.
	TLSConfig() (*tls.Config, error)
	// SSHConfig returns SSH configuration used to connect to Proxy, may be nil.
	SSHConfig() (*ssh.ClientConfig, error)
}

// LoadTLS is used to load credentials directly from another *tls.Config.
func LoadTLS(tlsConfig *tls.Config) *TLSConfigCreds {
	return &TLSConfigCreds{
		tlsConfig: tlsConfig,
	}
}

type TLSConfigCreds struct {
	tlsConfig *tls.Config
}

func (c *TLSConfigCreds) Dialer() (ContextDialer, error) {
	return nil, trace.NotImplemented("no dialer")
}

func (c *TLSConfigCreds) TLSConfig() (*tls.Config, error) {
	if c.tlsConfig == nil {
		return nil, trace.BadParameter("tls config is nil")
	}
	return configure(c.tlsConfig), nil
}

func (c *TLSConfigCreds) SSHConfig() (*ssh.ClientConfig, error) {
	return nil, nil
}

// LoadKeyPair is used to load credentials from files on disk.
func LoadKeyPair(certFile string, keyFile string, caFile string) *KeyPairCreds {
	return &KeyPairCreds{
		certFile: certFile,
		keyFile:  keyFile,
		caFile:   caFile,
	}
}

type KeyPairCreds struct {
	certFile string
	keyFile  string
	caFile   string
}

func (c *KeyPairCreds) Dialer() (ContextDialer, error) {
	return nil, trace.NotImplemented("no dialer")
}

func (c *KeyPairCreds) TLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cas, err := ioutil.ReadFile(c.caFile)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}

	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(cas); !ok {
		return nil, trace.BadParameter("invalid TLS CA cert PEM")
	}

	return configure(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}), nil
}

func (c *KeyPairCreds) SSHConfig() (*ssh.ClientConfig, error) {
	return nil, nil
}

// LoadIdentityFile is used to load credentials from an identity file on disk.
func LoadIdentityFile(path string) *IdentityCreds {
	return &IdentityCreds{
		path: path,
	}
}

type IdentityCreds struct {
	path string
}

func (c *IdentityCreds) Dialer() (ContextDialer, error) {
	return nil, trace.NotImplemented("no dialer")
}

func (c *IdentityCreds) TLSConfig() (*tls.Config, error) {
	identityFile, err := ReadIdentityFile(c.path)
	if err != nil {
		return nil, trace.BadParameter("identity file could not be decoded: %v", err)
	}

	tlsConfig, err := identityFile.TLSConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return configure(tlsConfig), nil
}

func (c *IdentityCreds) SSHConfig() (*ssh.ClientConfig, error) {
	identityFile, err := ReadIdentityFile(c.path)
	if err != nil {
		return nil, trace.BadParameter("identity file could not be decoded: %v", err)
	}

	sshConfig, err := identityFile.SSHClientConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return sshConfig, nil
}

func configure(c *tls.Config) *tls.Config {
	tlsConfig := c.Clone()

	tlsConfig.NextProtos = []string{http2.NextProtoTLS}

	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = constants.APIDomain
	}

	// This logic still appears to be necessary to force client to always send
	// a certificate regardless of the server setting. Otherwise the client may pick
	// not to send the client certificate by looking at certificate request.
	if len(tlsConfig.Certificates) > 0 {
		cert := tlsConfig.Certificates[0]
		tlsConfig.Certificates = nil
		tlsConfig.GetClientCertificate = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		}
	}

	return tlsConfig
}
