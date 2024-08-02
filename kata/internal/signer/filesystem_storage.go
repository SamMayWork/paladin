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

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-signer/pkg/keystorev3"
	"github.com/kaleido-io/paladin/kata/internal/cache"
	"github.com/kaleido-io/paladin/kata/internal/confutil"
	"github.com/kaleido-io/paladin/kata/internal/msgs"
	"github.com/kaleido-io/paladin/kata/internal/types"
	"github.com/kaleido-io/paladin/kata/pkg/proto"
)

type filesystemStorage struct {
	cache    cache.Cache[string, keystorev3.WalletFile]
	path     string
	fileMode os.FileMode
	dirMode  os.FileMode
}

func newFilesystemStorage(ctx context.Context, conf *FileSystemConfig) (fss CryptographicStorage, err error) {
	// Determine the path
	var pathInfo fs.FileInfo
	path, err := filepath.Abs(confutil.StringNotEmpty(conf.Path, *FileSystemDefaults.Path))
	if err == nil {
		pathInfo, err = os.Stat(path)
	}
	if err != nil || !pathInfo.IsDir() {
		return nil, i18n.WrapError(ctx, err, msgs.MsgSigningModuleBadPathError, *FileSystemDefaults.Path)
	}
	return &filesystemStorage{
		cache:    cache.NewCache[string, keystorev3.WalletFile](&conf.Cache, &FileSystemDefaults.Cache),
		fileMode: confutil.UnixFileMode(conf.FileMode, *FileSystemDefaults.FileMode),
		dirMode:  confutil.UnixFileMode(conf.DirMode, *FileSystemDefaults.DirMode),
		path:     path,
	}, nil
}

func (fss *filesystemStorage) validateFilePathKeyHandle(ctx context.Context, keyHandle string, forCreate bool) (absPath string, err error) {
	fullPath := fss.path
	segments := strings.Split(keyHandle, "/")
	for i, segment := range segments {
		isDir := i < (len(segments) - 1)

		// We use a file-or-directory prefix for two reasons:
		// - To avoid filesystem clashes between "something.key/another" and "something"
		// - Belt an braces to ensure we never use a ".anything" path segment
		if isDir {
			segment = "_" + segment
		} else {
			segment = "-" + segment
		}

		fullPath = path.Join(fullPath, segment)
		if forCreate {
			fsInfo, err := os.Stat(fullPath)
			if os.IsNotExist(err) {
				err = nil
				if isDir {
					err = os.Mkdir(fullPath, fss.dirMode)
				}
			} else {
				if (!isDir && fsInfo.IsDir()) || (isDir && !fsInfo.IsDir()) {
					err = i18n.NewError(ctx, msgs.MsgSigningModuleKeyHandleClash)
				}
			}
			if err != nil {
				return "", err
			}
		}
	}
	return fullPath, nil

}

func (fss *filesystemStorage) createWalletFile(ctx context.Context, keyFilePath, passwordFilePath string, newKeyMaterial func() []byte) (keystorev3.WalletFile, error) {

	password := types.RandHex(32)
	wf := keystorev3.NewWalletFileCustomBytesStandard(password, newKeyMaterial())

	err := os.WriteFile(passwordFilePath, []byte(password), fss.fileMode)
	if err == nil {
		err = os.WriteFile(keyFilePath, wf.JSON(), fss.fileMode)
	}
	if err != nil {
		return nil, i18n.WrapError(ctx, err, msgs.MsgSigningModuleFSError)
	}
	return wf, nil
}

func (fss *filesystemStorage) getOrCreateWalletFile(ctx context.Context, keyHandle string, newKeyMaterialFactory func() []byte) (keystorev3.WalletFile, error) {

	absPathPrefix, err := fss.validateFilePathKeyHandle(ctx, keyHandle, newKeyMaterialFactory != nil)
	if err != nil {
		return nil, err
	}

	cached, _ := fss.cache.Get(keyHandle)
	if cached != nil {
		return cached, nil
	}
	keyFilePath := fmt.Sprintf("%s.key", absPathPrefix)
	passwordFilePath := fmt.Sprintf("%s.pwd", absPathPrefix)

	_, checkNotExist := os.Stat(keyFilePath)
	if os.IsNotExist(checkNotExist) {
		if newKeyMaterialFactory != nil {
			// We need to create it
			wf, err := fss.createWalletFile(ctx, keyFilePath, passwordFilePath, newKeyMaterialFactory)
			if err == nil {
				fss.cache.Set(keyHandle, wf)
			}
			return wf, err
		} else {
			return nil, i18n.NewError(ctx, msgs.MsgSigningModuleKeyNotExist, keyHandle)
		}
	}
	// we need to read it
	wf, err := fss.readWalletFile(ctx, keyFilePath, passwordFilePath)
	if err == nil {
		fss.cache.Set(keyHandle, wf)
	}
	return wf, err
}

func (fss *filesystemStorage) readWalletFile(ctx context.Context, keyFilePath, passwordFilePath string) (keystorev3.WalletFile, error) {

	keyData, err := os.ReadFile(keyFilePath)
	if err != nil {
		return nil, i18n.WrapError(ctx, err, msgs.MsgSigningModuleBadKeyFile, keyFilePath)
	}

	passData, err := os.ReadFile(passwordFilePath)
	if err != nil {
		return nil, i18n.WrapError(ctx, err, msgs.MsgSigningModuleBadPassFile, passwordFilePath)
	}

	return keystorev3.ReadWalletFile(keyData, passData)
}

func (fss *filesystemStorage) FindOrCreateLoadableKey(ctx context.Context, req *proto.ResolveKeyRequest, newKeyMaterial func() []byte) (keyMaterial []byte, keyHandle string, err error) {
	if len(req.Path) == 0 {
		return nil, "", i18n.NewError(ctx, msgs.MsgSigningModuleBadKeyHandle)
	}
	for i, segment := range req.Path {
		if len(segment.Name) == 0 {
			return nil, "", i18n.NewError(ctx, msgs.MsgSigningModuleBadKeyHandle)
		}
		if i > 0 {
			keyHandle += "/"
		}
		keyHandle += url.PathEscape(segment.Name)
	}
	wf, err := fss.getOrCreateWalletFile(ctx, keyHandle, newKeyMaterial)
	if err != nil {
		return nil, "", err
	}
	return wf.PrivateKey(), keyHandle, nil
}

func (fss *filesystemStorage) LoadKeyMaterial(ctx context.Context, keyHandle string) ([]byte, error) {
	wf, err := fss.getOrCreateWalletFile(ctx, keyHandle, nil)
	if err != nil {
		return nil, err
	}
	return wf.PrivateKey(), nil
}
