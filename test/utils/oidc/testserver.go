/*
Copyright 2023 The Kubernetes Authors.

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

package oidc

import (
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"gopkg.in/square/go-jose.v2"
)

const (
	openIDWellKnownWebPath = "/.well-known/openid-configuration"
	authWebPath            = "/auth"
	tokenWebPath           = "/token"
	jwksWebPath            = "/jwks"
)

var (
	ErrRefreshTokenExpired = errors.New("refresh token is expired")
	ErrBadClientID         = errors.New("client ID is bad")
)

type TestServer struct {
	httpServer   *httptest.Server
	tokenHandler *MockTokenHandler
	jwksHandler  *MockJWKsHandler
}

// JwksHandler is getter of JSON Web Key Sets handler
func (ts *TestServer) JwksHandler() *MockJWKsHandler {
	return ts.jwksHandler
}

// TokenHandler is getter of JWT token handler
func (ts *TestServer) TokenHandler() *MockTokenHandler {
	return ts.tokenHandler
}

// URL returns the public URL of server
func (ts *TestServer) URL() string {
	return ts.httpServer.URL
}

// TokenURL returns the public URL of JWT token endpoint
func (ts *TestServer) TokenURL() (string, error) {
	url, err := url.JoinPath(ts.httpServer.URL, tokenWebPath)
	if err != nil {
		return "", fmt.Errorf("error joining paths: %v", err)
	}

	return url, nil
}

// BuildAndRunTestServer configures OIDC TLS server and its routing
func BuildAndRunTestServer(t *testing.T, caPath, caKeyPath string) *TestServer {
	t.Helper()

	certContent, err := os.ReadFile(caPath)
	require.NoError(t, err)
	keyContent, err := os.ReadFile(caKeyPath)
	require.NoError(t, err)

	cert, err := tls.X509KeyPair(certContent, keyContent)
	require.NoError(t, err)

	mux := http.NewServeMux()
	httpServer := httptest.NewUnstartedServer(mux)
	httpServer.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	httpServer.StartTLS()

	mockCtrl := gomock.NewController(t)

	t.Cleanup(func() {
		mockCtrl.Finish()
		httpServer.Close()
	})

	oidcServer := &TestServer{
		httpServer:   httpServer,
		tokenHandler: NewMockTokenHandler(mockCtrl),
		jwksHandler:  NewMockJWKsHandler(mockCtrl),
	}

	mux.HandleFunc(openIDWellKnownWebPath, func(writer http.ResponseWriter, request *http.Request) {
		authURL, err := url.JoinPath(httpServer.URL + authWebPath)
		require.NoError(t, err)
		tokenURL, err := url.JoinPath(httpServer.URL + tokenWebPath)
		require.NoError(t, err)
		jwksURL, err := url.JoinPath(httpServer.URL + jwksWebPath)
		require.NoError(t, err)
		userInfoURL, err := url.JoinPath(httpServer.URL + authWebPath)
		require.NoError(t, err)

		err = json.NewEncoder(writer).Encode(struct {
			Issuer      string `json:"issuer"`
			AuthURL     string `json:"authorization_endpoint"`
			TokenURL    string `json:"token_endpoint"`
			JWKSURL     string `json:"jwks_uri"`
			UserInfoURL string `json:"userinfo_endpoint"`
		}{
			Issuer:      httpServer.URL,
			AuthURL:     authURL,
			TokenURL:    tokenURL,
			JWKSURL:     jwksURL,
			UserInfoURL: userInfoURL,
		})
		require.NoError(t, err)

		writer.Header().Add("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc(tokenWebPath, func(writer http.ResponseWriter, request *http.Request) {
		token, err := oidcServer.tokenHandler.Token()
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}

		writer.Header().Add("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)

		err = json.NewEncoder(writer).Encode(token)
		require.NoError(t, err)
	})

	mux.HandleFunc(authWebPath, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc(jwksWebPath, func(writer http.ResponseWriter, request *http.Request) {
		keySet := oidcServer.jwksHandler.KeySet()

		writer.Header().Add("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)

		err := json.NewEncoder(writer).Encode(keySet)
		require.NoError(t, err)
	})

	return oidcServer
}

// TokenHandlerBehaviourReturningPredefinedJWT describes the scenario when signed JWT token is being created.
// This behaviour should being applied to the MockTokenHandler.
func TokenHandlerBehaviourReturningPredefinedJWT(
	t *testing.T,
	rsaPrivateKey *rsa.PrivateKey,
	issClaim,
	audClaim,
	subClaim,
	accessToken,
	refreshToken string,
	expClaim int64,
) func() (Token, error) {
	t.Helper()

	return func() (Token, error) {
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: rsaPrivateKey}, nil)
		require.NoError(t, err)

		payload := struct {
			Iss string `json:"iss"`
			Aud string `json:"aud"`
			Sub string `json:"sub"`
			Exp int64  `json:"exp"`
		}{
			Iss: issClaim,
			Aud: audClaim,
			Sub: subClaim,
			Exp: expClaim,
		}
		payloadJSON, err := json.Marshal(payload)
		require.NoError(t, err)

		idTokenSignature, err := signer.Sign(payloadJSON)
		require.NoError(t, err)
		idToken, err := idTokenSignature.CompactSerialize()
		require.NoError(t, err)

		return Token{
			IDToken:      idToken,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
		}, nil
	}
}

// DefaultJwksHandlerBehaviour describes the scenario when JSON Web Key Set token is being returned.
// This behaviour should being applied to the MockJWKsHandler.
func DefaultJwksHandlerBehaviour(t *testing.T, verificationPublicKey *rsa.PublicKey) func() jose.JSONWebKeySet {
	t.Helper()

	return func() jose.JSONWebKeySet {
		key := jose.JSONWebKey{Key: verificationPublicKey, Use: "sig", Algorithm: string(jose.RS256)}

		thumbprint, err := key.Thumbprint(crypto.SHA256)
		require.NoError(t, err)

		key.KeyID = hex.EncodeToString(thumbprint)
		return jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{key},
		}
	}
}
