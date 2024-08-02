/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package signer

import "github.com/kaleido-io/paladin/kata/internal/confutil"

type KeyStoreType string

const (
	KeyStoreTypeFilesystem KeyStoreType = "filesystem"
)

type Config struct {
	KeyStore      StoreConfig         `yaml:"keyStore"`
	KeyDerivation KeyDerivationConfig `yaml:"keyDerivation"`
}

type StoreConfig struct {
	Type              KeyStoreType     `yaml:"type"`
	DisableKeyListing bool             `yaml:"disableKeyListing"`
	DisableKeyLoading bool             `yaml:"disableKeyLoading"` // if HD Wallet or ZKP based signing is required, in-memory keys are required (so this needs to be false)
	FileSystem        FileSystemConfig `yaml:"filesystem"`
}

type KeyDerivationType string

const (
	// Direct uses a unique piece of key material in the storage for each key, for this signing module
	KeyDerivationTypeDirect KeyDerivationType = "direct"
	// Hierarchical uses a single BIP39 seed mnemonic in the storage, combined with a BIP32 wallet to derive keys
	KeyDerivationTypeHierarchical KeyDerivationType = "hierarchical"
)

type ConfigKeyPathEntry struct {
	Name       string            `yaml:"name"`
	Index      uint32            `yaml:"index"`
	Attributes map[string]string `yaml:"attributes"`
}

type KeyDerivationConfig struct {
	Type                  KeyDerivationType    `yaml:"type"`
	SeedKeyPath           []ConfigKeyPathEntry `yaml:"seedKeyHandle"`
	BIP44DirectResolution bool                 `yaml:"bip44DirectResolution"`
	BIP44Prefix           *string              `yaml:"bip44Prefix"`
	BIP44HardenedSegments *int                 `yaml:"bip44HardenedSegments"`
}

var KeyDerivationDefaults = &KeyDerivationConfig{
	BIP44Prefix:           confutil.P("m/44'/60'"),
	BIP44HardenedSegments: confutil.P(1), // in addition to the prefix, so `m/44'/60'/0'/0/0` for example with 3 segments, on top of the prefix
	SeedKeyPath: []ConfigKeyPathEntry{
		{
			Name:  "seed",
			Index: 0,
		},
	},
}
