// Copyright © 2024 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package msgs

import (
	"sync"

	"github.com/hyperledger/firefly-common/pkg/i18n"
	"golang.org/x/text/language"
)

var registered sync.Once
var ffe = func(key, translation string, statusHint ...int) i18n.ErrorMessageKey {
	registered.Do(func() {
		i18n.RegisterPrefix("PD03", "Paladin GRPC Transport")
	})
	return i18n.FFE(language.AmericanEnglish, key, translation, statusHint...)
}

var (
	// Generic PD0300XX
	MsgListenerPortAndAddressRequired       = ffe("PD030000", "port and address for listener are required")
	MsgInvalidTransportConfig               = ffe("PD030001", "Invalid transport configuration")
	MsgConfIncompatibleWithDirectCertVerify = ffe("PD030002", "When directCertVerification is enabled, TLS and clientAuth must be enabled, with no additional CA configuration or insecureSkipHostVerify")
	MsgInvalidSubjectRegexp                 = ffe("PD030003", "subjectMatchRegex is invalid")
	MsgVerifierRequiresOneCert              = ffe("PD030004", "certificate verifier expected exactly one certificate from peer certs=%d")
	MsgSubjectRegexpMismatch                = ffe("PD030005", "subjectMatchRegex did not match the subject in the certificate")
	MsgPeerTransportDetailsInvalid          = ffe("PD030006", "published peer transport details for node '%s' are invalid")
	MsgPeerCertificateIssuerInvalid         = ffe("PD030007", "peer '%s' did not provide a certificate signed an expected issuer received=%s issuers=%v")
	MsgTLSNegotiationFailed                 = ffe("PD030008", "TLS negotiation did not result in a verified peer node name")
	MsgAuthContextNotAvailable              = ffe("PD030009", "server failed to retrieve the auth context")
	MsgInvalidReplyToNode                   = ffe("PD030010", "replyTo node does not match sending node")
	MsgConnectionToWrongNode                = ffe("PD030011", "the TLS identity of the node '%s' does not match the expected node '%s'")
	MsgPEMCertificateInvalid                = ffe("PD030012", "invalid PEM encoded x509 certificate")
	MsgErrorNoTargetNode                    = ffe("PD030013", "request to send message but no target node specified")
)
