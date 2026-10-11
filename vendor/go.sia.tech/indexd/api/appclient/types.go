// Package appclient is the HTTP client for the indexer's application API. It
// deliberately contains no server code, so programs that only talk to the
// application API do not link the server handlers or the accounts, contracts
// and repair-metering packages they depend on.
package appclient

import (
	"errors"
	"time"

	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/api"
	"go.sia.tech/indexd/hosts"
)

// The following constants are the media types and signed-URL query parameter
// names shared between the client and the server.
const (
	ApplicationJSON        = "application/json"
	ApplicationOctetStream = "application/octet-stream"

	QueryParamCredential = "sc"
	QueryParamSignature  = "ss"
	QueryParamValidUntil = "sv"
)

type (
	// Info describes an application requesting a connection.
	Info struct {
		AppID       types.Hash256 `json:"appID"`
		Name        string        `json:"name"`
		Description string        `json:"description"`
		LogoURL     string        `json:"logoURL"`
		ServiceURL  string        `json:"serviceURL"`
		CallbackURL string        `json:"callbackURL"`
	}

	// A RegisterAppRequest is the request body for registering a new application.
	RegisterAppRequest struct {
		Info

		// PreAuthorizedKey and PreAuthorizationSignature allow the request to
		// bypass interactive approval when signed by a pre-authorized key.
		PreAuthorizedKey          types.PublicKey `json:"preAuthorizedKey,omitzero"`
		PreAuthorizationSignature types.Signature `json:"preAuthorizationSignature,omitzero"`
	}

	// AuthConnectStatusResponse is the response body for checking the status of an
	// application connection request.
	AuthConnectStatusResponse struct {
		Approved bool `json:"approved"`
		// Reconnecting is true if the user has previously connected the app.
		// It is only set if the user approved the request.
		Reconnecting bool          `json:"reconnecting"`
		UserSecret   types.Hash256 `json:"userSecret,omitzero"`
	}

	// RegisterAppResponse is the response body for registering a new application.
	// It contains the URL to redirect the user to for authentication.
	// The user must approve the request before the expiration time.
	RegisterAppResponse struct {
		ResponseURL string    `json:"responseURL"`
		StatusURL   string    `json:"statusURL"`
		RegisterURL string    `json:"registerURL"`
		Expiration  time.Time `json:"expiration"`
	}

	// AccountResponse is the response body for the [GET] /account endpoint.
	// It exposes the effective storage limit for the app together with the
	// remaining storage available under both the app limit and the quota.
	AccountResponse struct {
		AccountKey       proto.Account `json:"accountKey"`
		MaxPinnedData    uint64        `json:"maxPinnedData"`
		RemainingStorage uint64        `json:"remainingStorage"`
		Ready            bool          `json:"ready"`
		PinnedData       uint64        `json:"pinnedData"`
		PinnedSize       uint64        `json:"pinnedSize"`
		App              api.AppMeta   `json:"app"`
		LastUsed         time.Time     `json:"lastUsed"`
	}

	// RegisterAppKeyRequest is the request body for registering an application key after the user approves the connection request.
	// The request must be signed with the app key to prove ownership of it.
	RegisterAppKeyRequest struct {
		AppKey types.PublicKey `json:"appKey"`
		// Signature is a signature of the request ID signed with the app key to prove ownership
		Signature types.Signature `json:"signature"`
	}

	// ApproveAppRequest is the request body for approving or rejecting an
	// application connection request.
	ApproveAppRequest struct {
		Approve bool `json:"approve"`
	}

	// A SharedHost is a usable host paired with an account token the recipient
	// can use to pay for downloads from that host.
	SharedHost struct {
		hosts.HostInfo
		Token proto.AccountToken `json:"token"`
	}
)

// ErrUserRejected is returned when a user rejects an application connection
// request.
var ErrUserRejected = errors.New("user rejected connection request")

// RequestHash returns the hash that a signed request signs.
func RequestHash(method string, hostname, path string, validUntil time.Time, body []byte) types.Hash256 {
	h := types.NewHasher()
	h.E.Write([]byte(method))
	h.E.Write([]byte(hostname))
	h.E.Write([]byte(path))
	h.E.WriteUint64(uint64(validUntil.Unix()))
	if body != nil {
		h.E.Write(body)
	}
	return h.Sum()
}

// PreAuthorizationHash returns the proof hash signed by a pre-authorized key.
func PreAuthorizationHash(ephemeralKey types.PublicKey, request RegisterAppRequest) types.Hash256 {
	h := types.NewHasher()
	h.E.WriteString("indexd/preauthorize-app/v1")
	ephemeralKey.EncodeTo(h.E)
	request.AppID.EncodeTo(h.E)
	h.E.WriteString(request.Name)
	h.E.WriteString(request.Description)
	h.E.WriteString(request.LogoURL)
	h.E.WriteString(request.ServiceURL)
	h.E.WriteString(request.CallbackURL)
	request.PreAuthorizedKey.EncodeTo(h.E)
	return h.Sum()
}

// RegisterAppKeyHash returns the proof hash signed with an app key to prove
// ownership of it when registering.
func RegisterAppKeyHash(ephemeralKey types.PublicKey, requestID string) types.Hash256 {
	h := types.NewHasher()
	h.E.Write([]byte("registerAppKey"))
	h.E.Write(ephemeralKey[:])
	h.E.Write([]byte(requestID))
	return h.Sum()
}
