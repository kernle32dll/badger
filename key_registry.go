/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dgraph-io/badger/pb"
	"github.com/dgraph-io/badger/y"
)

const (
	// KeyRegistryFileName is the file name for the key registry file.
	KeyRegistryFileName = "KEYREGISTRY"
	// KeyRegistryRewriteFileName is the file name for the rewrite key registry file.
	KeyRegistryRewriteFileName = "REWRITE-KEYREGISTRY"
)

// SanityText is used to check whether the given user provided storage key is valid or not
var sanityText = []byte("Hello Badger")

// KeyRegistry used to maintain all the data keys.
type KeyRegistry struct {
	sync.RWMutex
	dataKeys    map[uint64]*pb.DataKey
	lastCreated int64 //lastCreated is the timestamp of the last data key generated.
	nextKeyID   uint64
	fp          *os.File
	opt         Options
}

// newKeyRegistry returns KeyRegistry.
func newKeyRegistry(opt Options) *KeyRegistry {
	return &KeyRegistry{
		dataKeys:  make(map[uint64]*pb.DataKey),
		nextKeyID: 0,
		opt:       opt,
	}
}

// OpenKeyRegistry opens key registry if it exists, otherwise it'll create key registry
// and returns key registry.
func OpenKeyRegistry(opt Options) (*KeyRegistry, error) {
	path := filepath.Join(opt.Dir, KeyRegistryFileName)
	var flags uint32
	if opt.ReadOnly {
		flags |= y.ReadOnly
	} else {
		flags |= y.Sync
	}
	fp, err := y.OpenExistingFile(path, flags)
	// OpenExistingFile just open file.
	// So checking whether the file exist or not. If not
	// We'll create new keyregistry.
	if err != nil && os.IsNotExist(err) {
		// Creating new registry file if not exist.
		kr := newKeyRegistry(opt)
		if opt.ReadOnly {
			return kr, nil
		}
		// Writing the key regitry to the file.
		if err := WriteKeyRegistry(kr, opt); err != nil {
			return nil, y.Wrapf(err, "Error while writing key registry.")
		}
		fp, err = y.OpenExistingFile(path, flags)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, y.Wrapf(err, "Error while opening key registry.")
	}
	kr, err := readKeyRegistry(fp, opt)
	if err != nil {
		// This case happens only if the file is opened properly and
		// not able to read.
		fp.Close()
		return nil, err
	}
	kr.fp = fp
	return kr, nil
}

// keyRegistryIterator reads all the datakey from the key registry
type keyRegistryIterator struct {
	encryptionKey []byte
	fp            *os.File
	// lenCrcBuf contains crc buf and data length to move forward.
	lenCrcBuf [8]byte
}

// newKeyRegistryIterator returns iterator, which will allow you to iterate
// over the data key of the the key registry.
func newKeyRegistryIterator(fp *os.File, encryptionKey []byte) (*keyRegistryIterator, error) {
	return &keyRegistryIterator{
		encryptionKey: encryptionKey,
		fp:            fp,
		lenCrcBuf:     [8]byte{},
	}, isValidRegistry(fp, encryptionKey)
}

// isValidRegistry checks the given encryption key is valid or not.
func isValidRegistry(fp *os.File, encryptionKey []byte) error {
	iv := make([]byte, aes.BlockSize)
	_, err := fp.Read(iv)
	if err != nil {
		return y.Wrapf(err, "Error while reading IV for ket registry.")
	}
	eSanityText := make([]byte, len(sanityText))
	if _, err = fp.Read(eSanityText); err != nil {
		return err
	}
	if len(encryptionKey) > 0 {
		// Decrypting sanity text.
		if eSanityText, err = y.XORBlock(eSanityText, encryptionKey, iv); err != nil {
			return err
		}
	}
	// Check the given key is valid or not.
	if !bytes.Equal(eSanityText, sanityText) {
		return ErrEncryptionKeyMismatch
	}
	return nil
}

func (kri *keyRegistryIterator) Next() (*pb.DataKey, error) {
	// isEOF returns nil if it is EOF.
	isEOF := func(err error) error {
		if err == io.EOF {
			return nil
		}
		return err
	}
	var err error
	// Read crc buf and data length.
	if _, err = kri.fp.Read(kri.lenCrcBuf[:]); err != nil {
		return nil, isEOF(err)
	}
	l := int64(binary.BigEndian.Uint32(kri.lenCrcBuf[0:4]))
	// Read protobuf data.
	data := make([]byte, l)
	if _, err = kri.fp.Read(data); err != nil {
		return nil, isEOF(err)
	}
	// Check checksum.
	if crc32.Checksum(data, y.CastagnoliCrcTable) != binary.BigEndian.Uint32(kri.lenCrcBuf[4:]) {
		return nil, errBadChecksum
	}
	dataKey := &pb.DataKey{}
	if err = dataKey.Unmarshal(data); err != nil {
		return nil, err
	}
	if len(kri.encryptionKey) > 0 {
		// Decrypt the key if the storage key exits.
		if dataKey.Data, err = y.XORBlock(dataKey.Data, kri.encryptionKey, dataKey.Iv); err != nil {
			return nil, err
		}
	}
	return dataKey, nil
}

// readKeyRegistry will read the key registry file and build the key registry struct.
func readKeyRegistry(fp *os.File, opt Options) (*KeyRegistry, error) {
	itr, err := newKeyRegistryIterator(fp, opt.EncryptionKey)
	if err != nil {
		return nil, err
	}
	kr := newKeyRegistry(opt)
	var dk *pb.DataKey
	dk, err = itr.Next()
	for err == nil && dk != nil {
		if dk.KeyId > kr.nextKeyID {
			// Set the maximum key ID for next key ID generation.
			kr.nextKeyID = dk.KeyId
		}
		if dk.CreatedAt > kr.lastCreated {
			// Set the last generated key timestamp.
			kr.lastCreated = dk.CreatedAt
		}
		// No need to lock, since we are building the initial state.
		kr.dataKeys[kr.nextKeyID] = dk
		// Forward the iterator.
		dk, err = itr.Next()
	}
	return kr, err
}

/*
Structure of Key Registry.
+-------------------+---------------------+--------------------+--------------+------------------+
| Sanity Text       | IV                  | DataKey1           | DataKey2     | ...              |
+-------------------+---------------------+--------------------+--------------+------------------+
*/

// WriteKeyRegistry will rewrite the existing key registry file with new one
// It is okay to give closed key registry. Since, it's using only the datakey.
func WriteKeyRegistry(reg *KeyRegistry, opt Options) error {
	tmpPath := filepath.Join(opt.Dir, KeyRegistryRewriteFileName)
	// Open temporary file to write the data and do atomic rename.
	fp, err := y.OpenTruncFile(tmpPath, true)
	if err != nil {
		return err
	}
	// closeBeforeReturn will close the fd before returing error.
	closeBeforeReturn := func(err error) error {
		fp.Close()
		return err
	}
	buf := &bytes.Buffer{}
	iv, err := y.GenerateIV()
	if err != nil {
		return closeBeforeReturn(err)
	}
	// Encrypt sanity text if the storage presents.
	eSanity := sanityText
	if len(opt.EncryptionKey) > 0 {
		var err error
		eSanity, err = y.XORBlock(eSanity, opt.EncryptionKey, iv)
		if err != nil {
			return closeBeforeReturn(err)
		}
	}
	_, err = buf.Write(iv)
	y.Check(err)
	_, err = buf.Write(eSanity)
	y.Check(err)
	// Write all the datakeys to the buf.
	for _, k := range reg.dataKeys {
		// Writing the datakey to the given file fd.
		if err := storeDataKey(buf, opt.EncryptionKey, k); err != nil {
			return closeBeforeReturn(err)
		}
	}
	// Write buf to the disk.
	if _, err = fp.Write(buf.Bytes()); err != nil {
		return closeBeforeReturn(err)
	}
	// We need to close the file before renaming.
	if err = fp.Close(); err != nil {
		return err
	}
	// Rename to the original file.
	if err = os.Rename(tmpPath, filepath.Join(opt.Dir, KeyRegistryFileName)); err != nil {
		return err
	}
	// Sync Dir.
	return syncDir(opt.Dir)
}

func (kr *KeyRegistry) dataKey(id uint64) (*pb.DataKey, error) {
	if id == 0 {
		// nil represent plain text.
		return nil, nil
	}
	dk, ok := kr.dataKeys[id]
	if !ok {
		return nil, y.Wrapf(ErrInvalidDataKeyID, "Error for the KEY ID %d", id)
	}
	return dk, nil
}

// latest datakey will give you the lastest generated datakey based on the rotation
// period. If the last generated datakey lifetime exceeds more than the rotation period.
// It'll create new datakey.
func (kr *KeyRegistry) latestDataKey() (*pb.DataKey, error) {
	if len(kr.opt.EncryptionKey) == 0 {
		return nil, nil
	}
	// Time diffrence from the last generated time.
	diff := time.Since(time.Unix(kr.lastCreated, 0))
	if diff < kr.opt.EncryptionKeyRotationDuration {
		// If less than EncryptionKeyRotationDuration, returns the last generaterd key.
		kr.RLock()
		defer kr.RUnlock()
		dk := kr.dataKeys[kr.nextKeyID]
		return dk, nil
	}
	kr.Lock()
	defer kr.Unlock()
	// Otherwise Increment the KeyID and generate new datakey.
	kr.nextKeyID++
	k := make([]byte, len(kr.opt.EncryptionKey))
	iv, err := y.GenerateIV()
	if err != nil {
		return nil, err
	}
	_, err = rand.Read(k)
	if err != nil {
		return nil, err
	}
	dk := &pb.DataKey{
		KeyId:     kr.nextKeyID,
		Data:      k,
		CreatedAt: time.Now().Unix(),
		Iv:        iv,
	}
	// Store the datekey.
	buf := &bytes.Buffer{}
	if err = storeDataKey(buf, kr.opt.EncryptionKey, dk); err != nil {
		return nil, err
	}
	// PeEntry5rsist the datakey to the disk
	if _, err = kr.fp.Write(buf.Bytes()); err != nil {
		return nil, err
	}
	// storeDatakey encrypts the datakey So, placing unencrypted key in the memory.
	dk.Data = k
	kr.lastCreated = dk.CreatedAt
	kr.dataKeys[kr.nextKeyID] = dk
	return dk, nil
}

// Close closes the key registry.
func (kr *KeyRegistry) Close() error {
	return kr.fp.Close()
}

// storeDataKey stores datakey in a encrypted format in the given buffer. If storage key preset.
func storeDataKey(buf *bytes.Buffer, storageKey []byte, k *pb.DataKey) error {
	// xor will encrypt the IV and xor with the given data.
	// It'll used for both encryption and decryption.
	xor := func() error {
		if len(storageKey) == 0 {
			return nil
		}
		var err error
		k.Data, err = y.XORBlock(k.Data, storageKey, k.Iv)
		return err
	}
	// In memory datakey will in plain text, so encrypting before storing to the disk.
	var err error
	if err = xor(); err != nil {
		return err
	}
	var data []byte
	if data, err = k.Marshal(); err != nil {
		return err
	}
	var lenCrcBuf [8]byte
	binary.BigEndian.PutUint32(lenCrcBuf[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(lenCrcBuf[4:8], crc32.Checksum(data, y.CastagnoliCrcTable))
	_, err = buf.Write(lenCrcBuf[:])
	y.Check(err)
	_, err = buf.Write(data)
	y.Check(err)
	// Decrypting the datakey back since we're using the pointer.
	return xor()
}