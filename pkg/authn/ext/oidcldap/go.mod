module github.com/cocomhub/sproxy/pkg/authn/ext/oidcldap

go 1.27

replace github.com/cocomhub/sproxy => ../../../..

require (
	github.com/cocomhub/sproxy v0.0.0
	github.com/go-ldap/ldap/v3 v3.4.14
	golang.org/x/oauth2 v0.37.0
)

require (
	github.com/Azure/go-ntlmssp v0.1.1 // indirect
	github.com/go-asn1-ber/asn1-ber v1.5.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
)
