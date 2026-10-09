package model

import "fmt"

// UpgradeSchema is deliberately limited to the known schema-4 predecessor.
// It only normalizes in memory; the next explicit save writes schema 5.
func UpgradeSchema(s *State) error {
	if s.SchemaVersion == 5 {
		s.SchemaVersion = SchemaVersion
		return nil
	}
	if s.SchemaVersion != 4 {
		return nil
	}
	if len(s.CloudflareAccounts) != 0 || len(s.DNSBindings) != 0 {
		return fmt.Errorf("schema 4 cannot contain schema 5 cloud settings")
	}
	for _, service := range s.Services {
		if service.EdgeProvider != "" || service.EdgeAccountID != "" || service.UsePreferred || service.PreferredDomain != "" {
			return fmt.Errorf("schema 4 cannot contain provider-specific service settings")
		}
	}
	for i := range s.DNSAccounts {
		if s.DNSAccounts[i].Provider == "" {
			s.DNSAccounts[i].Provider = "dnspod"
		}
	}
	s.SchemaVersion = SchemaVersion
	return nil
}

// DNSBinding makes ownership explicit when multiple enabled accounts can see a zone.
type DNSBinding struct {
	Exact     bool   `json:"exact,omitempty"`
	Automatic bool   `json:"automatic,omitempty"`
	Host      string `json:"host"`
	AccountID string `json:"accountId"`
	Zone      string `json:"zone"`
}

// CloudflareAccount is a SaaS profile, independent of any business DNS account.
// Its bearer token is stored only in a private credential file.
type CloudflareAccount struct {
	CredentialFile    string `json:"credentialFile,omitempty"`
	ID                string `json:"id"`
	Name              string `json:"name"`
	ZoneID            string `json:"zoneId"`
	ZoneName          string `json:"zoneName"`
	FallbackDomain    string `json:"fallbackDomain"`
	StandardTarget    string `json:"standardTarget"`
	TokenHint         string `json:"tokenHint"`
	CredentialVersion int64  `json:"credentialVersion"`
	Enabled           bool   `json:"enabled"`
	AutoSSLStrict     bool   `json:"autoSSLStrict,omitempty"`
}
