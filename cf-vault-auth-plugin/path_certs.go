package main

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/vault/helper/policyutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

func pathListCerts(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "certs/?",

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.ListOperation: b.pathCertList,
		},

		HelpSynopsis:    pathCertHelpSyn,
		HelpDescription: pathCertHelpDesc,
	}
}

func pathCerts(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "certs/" + framework.GenericNameRegex("name"),
		Fields: map[string]*framework.FieldSchema{
			"name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "The name of the certificate",
			},

			"certificate": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `The public certificate that should be trusted.
Must be x509 PEM encoded.`,
			},

			"display_name": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `The display name to use for clients using this
certificate.`,
			},

			"policies": &framework.FieldSchema{
				Type:        framework.TypeCommaStringSlice,
				Description: "Comma-separated list of policies.",
			},

			"ttl": &framework.FieldSchema{
				Type: framework.TypeDurationSecond,
				Description: `TTL for tokens issued by this backend.
Defaults to system/backend default TTL time.`,
			},

			"max_ttl": &framework.FieldSchema{
				Type: framework.TypeDurationSecond,
				Description: `Duration in either an integer number of seconds (3600) or
an integer time unit (60m) after which the
issued token can no longer be renewed.`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.DeleteOperation: b.pathCertDelete,
			logical.ReadOperation:   b.pathCertRead,
			logical.UpdateOperation: b.pathCertWrite,
		},

		HelpSynopsis:    pathCertHelpSyn,
		HelpDescription: pathCertHelpDesc,
	}
}

func (b *backend) Cert(ctx context.Context, s logical.Storage, n string) (*CertEntry, error) {
	entry, err := s.Get(ctx, "cert/"+strings.ToLower(n))
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}

	var result CertEntry
	if err := entry.DecodeJSON(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (b *backend) pathCertDelete(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	err := req.Storage.Delete(ctx, "cert/"+strings.ToLower(d.Get("name").(string)))
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (b *backend) pathCertList(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	certs, err := req.Storage.List(ctx, "cert/")
	if err != nil {
		return nil, err
	}
	return logical.ListResponse(certs), nil
}

func (b *backend) pathCertRead(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	cert, err := b.Cert(ctx, req.Storage, strings.ToLower(d.Get("name").(string)))
	if err != nil {
		return nil, err
	}
	if cert == nil {
		return nil, nil
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"certificate":  cert.Certificate,
			"display_name": cert.DisplayName,
			"policies":     cert.Policies,
			"ttl":          cert.TTL / time.Second,
			"max_ttl":      cert.MaxTTL / time.Second,
		},
	}, nil
}

func (b *backend) pathCertWrite(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	b.Logger().Info("actually doing something")

	name := strings.ToLower(d.Get("name").(string))
	certificate := d.Get("certificate").(string)
	displayName := d.Get("display_name").(string)
	policies := policyutil.ParsePolicies(d.Get("policies"))

	b.Logger().Info("parsed something")

	var resp logical.Response

	// Parse the ttl (or lease duration)
	systemDefaultTTL := b.System().DefaultLeaseTTL()
	ttl := time.Duration(d.Get("ttl").(int)) * time.Second

	if ttl > systemDefaultTTL {
		resp.AddWarning(fmt.Sprintf("Given ttl of %d seconds is greater than current mount/system default of %d seconds", ttl/time.Second, systemDefaultTTL/time.Second))
	}

	if ttl < time.Duration(0) {
		return logical.ErrorResponse("ttl cannot be negative"), nil
	}

	// Parse max_ttl
	systemMaxTTL := b.System().MaxLeaseTTL()
	maxTTL := time.Duration(d.Get("max_ttl").(int)) * time.Second
	if maxTTL > systemMaxTTL {
		resp.AddWarning(fmt.Sprintf("Given max_ttl of %d seconds is greater than current mount/system default of %d seconds", maxTTL/time.Second, systemMaxTTL/time.Second))
	}

	if maxTTL < time.Duration(0) {
		return logical.ErrorResponse("max_ttl cannot be negative"), nil
	}

	if maxTTL != 0 && ttl > maxTTL {
		return logical.ErrorResponse("ttl should be shorter than max_ttl"), nil
	}

	// Default the display name to the certificate name if not given
	if displayName == "" {
		displayName = name
	}

	b.Logger().Info("parsing pem")

	parsed := parsePEM([]byte(certificate))
	if len(parsed) == 0 {
		return logical.ErrorResponse("failed to parse certificate"), nil
	}

	b.Logger().Info("parsed pem")

	// If the certificate is not a CA cert, then ensure that x509.ExtKeyUsageClientAuth is set
	if !parsed[0].IsCA && parsed[0].ExtKeyUsage != nil {
		var clientAuth bool
		for _, usage := range parsed[0].ExtKeyUsage {
			if usage == x509.ExtKeyUsageClientAuth || usage == x509.ExtKeyUsageAny {
				clientAuth = true
				break
			}
		}
		if !clientAuth {
			return logical.ErrorResponse("non-CA certificates should have TLS client authentication set as an extended key usage"), nil
		}
	}

	certEntry := &CertEntry{
		Name:        name,
		Certificate: certificate,
		DisplayName: displayName,
		Policies:    policies,
		TTL:         ttl,
		MaxTTL:      maxTTL,
	}

	// Store it
	entry, err := logical.StorageEntryJSON("cert/"+name, certEntry)
	if err != nil {
		return nil, err
	}
	if err := req.Storage.Put(ctx, entry); err != nil {
		return nil, err
	}

	if len(resp.Warnings) == 0 {
		return nil, nil
	}

	return &resp, nil
}

type CertEntry struct {
	Name        string
	Certificate string
	DisplayName string
	Policies    []string
	TTL         time.Duration
	MaxTTL      time.Duration
}

const pathCertHelpSyn = `
Manage trusted certificates used for authentication.
`

const pathCertHelpDesc = `
This endpoint allows you to create, read, update, and delete trusted certificates
that are allowed to authenticate.

Deleting a certificate will not revoke auth for prior authenticated connections.
To do this, do a revoke on "login". If you don't need to revoke login immediately,
then the next renew will cause the lease to expire.
`
