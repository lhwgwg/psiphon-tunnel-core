/*
 * Copyright (c) 2019, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"encoding/base64"
	"encoding/json"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/crypto/nacl/secretbox"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/errors"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

// ExportExchangePayload creates a payload for client-to-client server
// connection info exchange. The payload includes the most recent successful
// server entry -- the server entry in the affinity position -- and any
// associated dial parameters, for the current network ID.
//
// ExportExchangePayload is intended to be called when the client is
// connected, as the affinity server will be the currently connected server
// and there will be dial parameters for the current network ID.
//
// Only signed server entries will be exchanged. The signature is created by
// the Psiphon Network and may be verified using the
// ServerEntrySignaturePublicKey embedded in clients. This signture defends
// against attacks by rogue clients and man-in-the-middle operatives which
// could otherwise cause the importer to receive phony server entry values.
//
// Only a subset of dial parameters are exchanged. See the comment for
// ExchangedDialParameters for more details. When no dial parameters is
// present the exchange proceeds without dial parameters.
//
// The exchange payload is obfuscated with the ExchangeObfuscationKey embedded
// in clients. The purpose of this obfuscation is to ensure that plaintext
// server entry info cannot be trivially exported and displayed or published;
// or at least require an effort equal to what's required without the export
// feature.
//
// There is no success notice for exchange ExportExchangePayload (or
// ImportExchangePayload) as this would potentially leak a user releationship if
// two users performed and exchange and subseqently submit diagnostic feedback
// containg import and export logs at almost the same point in time, along
// with logs showing connections to the same server, with source "EXCHANGED"
// in the importer case.
//
// Failure notices are logged as, presumably, the event will only appear on
// one end of the exchange and the error is potentially important diagnostics.
//
// There remains some risk of user linkability from Connecting/ConnectedServer
// diagnostics and metrics alone, because the appearance of "EXCHANGED" may
// indicate an exchange event. But there are various degrees of ambiguity in
// this case in terms of determining the server entry was freshly exchanged;
// and with likely many users often connecting to any given server in a short
// time period.
//
// The return value is a payload that may be exchanged with another client;
// when "", the export failed and a diagnostic notice has been logged.
func ExportExchangePayload(config *Config) string {
	payload, err := exportExchangePayload(config)
	if err != nil {
		NoticeWarning("ExportExchangePayload failed: %s", errors.Trace(err))
		return ""
	}
	return payload
}

// ImportExchangePayload imports a payload generated by ExportExchangePayload.
// The server entry in the payload is promoted to the affinity position so it
// will be the first candidate in any establishment that begins after the
// import.
//
// The current network ID. This may not be the same network as the exporter,
// even if the client-to-client exchange occurs in real time. For example, if
// the exchange is performed over NFC between two devices, they may be on
// different mobile or WiFi networks. As mentioned in the comment for
// ExchangedDialParameters, the exchange dial parameters includes only the
// most broadly applicable fields.
//
// The return value indicates a successful import. If the import failed, a
// a diagnostic notice has been logged.
func ImportExchangePayload(config *Config, encodedPayload string) bool {
	err := importExchangePayload(config, encodedPayload)
	if err != nil {
		NoticeWarning("ImportExchangePayload failed: %s", errors.Trace(err))
		return false
	}
	return true
}

type exchangePayload struct {
	ServerEntryFields       protocol.ServerEntryFields
	ExchangedDialParameters *ExchangedDialParameters
}

func exportExchangePayload(config *Config) (string, error) {

	networkID := config.GetNetworkID()

	key, err := getExchangeObfuscationKey(config)
	if err != nil {
		return "", errors.Trace(err)
	}

	serverEntryFields, dialParams, err :=
		GetAffinityServerEntryAndDialParameters(networkID)
	if err != nil {
		return "", errors.Trace(err)
	}

	// Fail if the server entry has no signature, as the exchange would be
	// insecure. Given the mechanism where handshake will return a signed server
	// entry to clients without one, this case is not expected to occur.
	if !serverEntryFields.HasSignature() {
		return "", errors.TraceNew("export server entry not signed")
	}

	// RemoveUnsignedFields also removes potentially sensitive local fields, so
	// explicitly strip these before exchanging.
	serverEntryFields.RemoveUnsignedFields()

	var exchangedDialParameters *ExchangedDialParameters
	if dialParams != nil {
		exchangedDialParameters = NewExchangedDialParameters(dialParams)
	}

	payload := &exchangePayload{
		ServerEntryFields:       serverEntryFields,
		ExchangedDialParameters: exchangedDialParameters,
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", errors.Trace(err)
	}

	// A unique nonce is generated and included with the payload as the
	// obfuscation keys is not single-use.
	nonce, err := common.MakeSecureRandomBytes(24)
	if err != nil {
		return "", errors.Trace(err)
	}

	var secretboxNonce [24]byte
	copy(secretboxNonce[:], nonce)
	var secretboxKey [32]byte
	copy(secretboxKey[:], key)
	boxedPayload := secretbox.Seal(
		nil, payloadJSON, &secretboxNonce, &secretboxKey)
	boxedPayload = append(secretboxNonce[:], boxedPayload...)

	return base64.StdEncoding.EncodeToString(boxedPayload), nil
}

func importExchangePayload(config *Config, encodedPayload string) error {

	networkID := config.GetNetworkID()

	key, err := getExchangeObfuscationKey(config)
	if err != nil {
		return errors.Trace(err)
	}

	boxedPayload, err := base64.StdEncoding.DecodeString(encodedPayload)
	if err != nil {
		return errors.Trace(err)
	}

	if len(boxedPayload) <= 24 {
		return errors.TraceNew("unexpected box length")
	}

	var secretboxNonce [24]byte
	copy(secretboxNonce[:], boxedPayload[:24])
	var secretboxKey [32]byte
	copy(secretboxKey[:], key)
	payloadJSON, ok := secretbox.Open(
		nil, boxedPayload[24:], &secretboxNonce, &secretboxKey)
	if !ok {
		return errors.TraceNew("unbox failed")
	}

	var payload *exchangePayload
	err = json.Unmarshal(payloadJSON, &payload)
	if err != nil {
		return errors.Trace(err)
	}

	// Explicitly strip any unsigned fields that should not be exchanged or
	// imported.
	payload.ServerEntryFields.RemoveUnsignedFields()

	err = payload.ServerEntryFields.VerifySignature(
		config.ServerEntrySignaturePublicKey)
	if err != nil {
		return errors.Trace(err)
	}

	payload.ServerEntryFields.SetLocalSource(
		protocol.SERVER_ENTRY_SOURCE_EXCHANGED)
	payload.ServerEntryFields.SetLocalTimestamp(
		common.TruncateTimestampToHour(common.GetCurrentTimestamp()))

	// The following sequence of datastore calls -- StoreServerEntry,
	// PromoteServerEntry, SetDialParameters -- is not an atomic transaction but
	// the  datastore will end up in a consistent state in case of failure to
	// complete the sequence. The existing calls are reused to avoid redundant
	// code.
	//
	// TODO: refactor existing code to allow reuse in a single transaction?

	err = StoreServerEntry(payload.ServerEntryFields, true)
	if err != nil {
		return errors.Trace(err)
	}

	err = PromoteServerEntry(config, payload.ServerEntryFields.GetIPAddress())
	if err != nil {
		return errors.Trace(err)
	}

	if payload.ExchangedDialParameters != nil {

		serverEntry, err := payload.ServerEntryFields.GetServerEntry()
		if err != nil {
			return errors.Trace(err)
		}

		// Don't abort if Validate fails, as the current client may simply not
		// support the exchanged dial parameter values (for example, a new tunnel
		// protocol).
		//
		// No notice is issued in the error case for the give linkage reason, as the
		// notice would be a proxy for an import success log.

		err = payload.ExchangedDialParameters.Validate(serverEntry)
		if err == nil {
			dialParams := payload.ExchangedDialParameters.MakeDialParameters(
				config,
				config.GetClientParameters().Get(),
				serverEntry)

			err = SetDialParameters(
				payload.ServerEntryFields.GetIPAddress(),
				networkID,
				dialParams)
			if err != nil {
				return errors.Trace(err)
			}
		}
	}

	return nil
}

func getExchangeObfuscationKey(config *Config) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(config.ExchangeObfuscationKey)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(key) != 32 {
		return nil, errors.TraceNew("invalid key size")
	}
	return key, nil
}
