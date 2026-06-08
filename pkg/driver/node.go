package driver

import (
	internalDriver "github.com/topolvm/topolvm/internal/driver"
)

var NewNodeServer = internalDriver.NewNodeServer

// NewNodeServerWithEncryption is the encryption-aware NodeServer constructor.
var NewNodeServerWithEncryption = internalDriver.NewNodeServerWithEncryption

// EncryptionDeps is the surface the node binary uses to opt into TDE.
type EncryptionDeps = internalDriver.EncryptionDeps
