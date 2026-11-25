package db

import (
	"database/sql"

	"github.com/Autumn-27/artex/oauth"
)

var (
	colOAuthAccess  = secretColumn{"oauth_credentials", "access_token", false, ""}
	colOAuthRefresh = secretColumn{"oauth_credentials", "refresh_token", false, ""}
)

// GetOAuthCredential loads and decrypts one provider's OAuth credential, or nil if absent.
func (d *DB) GetOAuthCredential(provider string) (*oauth.Credential, error) {
	var c oauth.Credential
	err := d.QueryRow(
		`SELECT provider, client_id, access_token, refresh_token, id_token, account_hint, scopes, expires_at, updated_at
		 FROM oauth_credentials WHERE provider=$1`, provider,
	).Scan(&c.Provider, &c.ClientID, &c.AccessToken, &c.RefreshToken, &c.IDToken, &c.AccountHint, &c.Scopes, &c.ExpiresAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.AccessToken, err = openSecret(colOAuthAccess, c.AccessToken); err != nil {
		return nil, err
	}
	if c.RefreshToken, err = openSecret(colOAuthRefresh, c.RefreshToken); err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertOAuthCredential encrypts and stores one provider's OAuth credential.
func (d *DB) UpsertOAuthCredential(c *oauth.Credential) error {
	access, err := sealSecret(colOAuthAccess, c.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := sealSecret(colOAuthRefresh, c.RefreshToken)
	if err != nil {
		return err
	}
	_, err = d.Exec(
		`INSERT INTO oauth_credentials(provider, client_id, access_token, refresh_token, id_token, account_hint, scopes, expires_at, updated_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (provider) DO UPDATE SET
		   client_id=EXCLUDED.client_id,
		   access_token=EXCLUDED.access_token,
		   refresh_token=EXCLUDED.refresh_token,
		   id_token=EXCLUDED.id_token,
		   account_hint=EXCLUDED.account_hint,
		   scopes=EXCLUDED.scopes,
		   expires_at=EXCLUDED.expires_at,
		   updated_at=EXCLUDED.updated_at`,
		c.Provider, c.ClientID, access, refresh, c.IDToken, c.AccountHint, c.Scopes, c.ExpiresAt, c.UpdatedAt,
	)
	return err
}

// DeleteOAuthCredential removes one provider's OAuth credential.
func (d *DB) DeleteOAuthCredential(provider string) error {
	_, err := d.Exec(`DELETE FROM oauth_credentials WHERE provider=$1`, provider)
	return err
}
