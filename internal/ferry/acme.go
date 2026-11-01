package ferry

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/gcore"
	"github.com/go-acme/lego/v4/registration"
)

// certIssuer signs a certificate for one domain.
type certIssuer interface {
	Issue(ctx context.Context, domain string) (certPEM, keyPEM string, err error)
}

// legoIssuer gets certificates from Let's Encrypt's production directory, answering DNS-01 through
// Gcore. Each order registers a fresh account: nothing has to survive between runs.
type legoIssuer struct {
	email string
	token string
}

// Issue gives up when ctx ends, but lego can't be interrupted: its goroutine runs out on its own.
func (l legoIssuer) Issue(ctx context.Context, domain string) (string, string, error) {
	type result struct {
		cert, key string
		err       error
	}
	done := make(chan result, 1)
	go func() {
		cert, key, err := l.obtain(domain)
		done <- result{cert, key, err}
	}()
	select {
	case r := <-done:
		return r.cert, r.key, r.err
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func (l legoIssuer) obtain(domain string) (string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	user := &acmeUser{email: l.email, key: key}
	client, err := lego.NewClient(lego.NewConfig(user))
	if err != nil {
		return "", "", fmt.Errorf("acme client: %w", err)
	}
	dnsCfg := gcore.NewDefaultConfig()
	dnsCfg.APIToken = l.token
	provider, err := gcore.NewDNSProviderConfig(dnsCfg)
	if err != nil {
		return "", "", err
	}
	if err := client.Challenge.SetDNS01Provider(provider); err != nil {
		return "", "", err
	}
	user.registration, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return "", "", fmt.Errorf("acme account: %w", err)
	}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{domain}, Bundle: true})
	if err != nil {
		return "", "", fmt.Errorf("acme order: %w", err)
	}
	return string(res.Certificate), string(res.PrivateKey), nil
}

type acmeUser struct {
	email        string
	key          crypto.PrivateKey
	registration *registration.Resource
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.registration }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }
