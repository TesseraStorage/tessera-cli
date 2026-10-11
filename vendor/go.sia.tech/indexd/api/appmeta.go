package api

import "go.sia.tech/core/types"

// AppMeta contains additional metadata associated with an application account.
// It is the payload of the app field in AccountResponse and is defined here so
// both the accounts store and the application API client can use it without
// the client linking the accounts package.
type AppMeta struct {
	ID          types.Hash256 `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	LogoURL     string        `json:"logoURL"`
	ServiceURL  string        `json:"serviceURL"`
}
