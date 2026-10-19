package secrets

// Additional data (AAD) binds a sealed value to the row it belongs to: a
// ciphertext copied into another row, or another column, does not open. The
// strings are part of the stored data, so they never change; every package that
// seals or opens a value, and the key rotation that rewrites them all, builds
// them here.

// AADEndpointSecret is the additional data of a webhook endpoint's signing secret.
func AADEndpointSecret(endpointID string) []byte {
	return []byte("webhook_endpoints.signing_secret:" + endpointID)
}

// AADProviderConfig is the additional data of a provider account's sealed config.
func AADProviderConfig(accountID string) []byte {
	return []byte("provider_accounts.config_enc:" + accountID)
}

// AADTOTP is the additional data of a user's TOTP secret.
func AADTOTP(userID string) []byte { return []byte("users.totp_secret:" + userID) }

// AADBeneficiary is the additional data of a payout's beneficiary details.
func AADBeneficiary(payoutID string) []byte { return []byte("payouts.beneficiary:" + payoutID) }

// AADTaxEntityDSC is the additional data of a tax entity's digital signature certificate.
func AADTaxEntityDSC(entityID string) []byte { return []byte("tax_entities.dsc_enc:" + entityID) }

// AADConnectorKV is the additional data of a value a connector keeps for its account.
func AADConnectorKV(accountID, key string) []byte {
	return []byte("connector_kv:" + accountID + ":" + key)
}

// AADIdempotency is the additional data of a stored idempotent response.
func AADIdempotency(appID, mode, key string) []byte { return []byte(appID + "/" + mode + "/" + key) }

// KeyIDOf reads the id of the key a ciphertext was sealed under.
func KeyIDOf(ciphertext []byte) (string, bool) {
	if len(ciphertext) < 2 || ciphertext[0] != formatV1 {
		return "", false
	}
	n := int(ciphertext[1])
	if len(ciphertext) < 2+n {
		return "", false
	}
	return string(ciphertext[2 : 2+n]), true
}

// ActiveKeyID is the id of the key new values are sealed under.
func (b *Box) ActiveKeyID() string { return b.activeID }
