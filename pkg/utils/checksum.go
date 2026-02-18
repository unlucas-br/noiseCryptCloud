package utils

import (
	"crypto/sha256"
	"hash/crc32"
)

// SHA256 computes SHA-256 hash of data
func SHA256(data []byte) [32]byte {
	return sha256.Sum256(data)
}

// CRC32 computes CRC32 checksum of data
func CRC32(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}

// VerifySHA256 checks if data matches expected hash
func VerifySHA256(data []byte, expected [32]byte) bool {
	actual := SHA256(data)
	return actual == expected
}

// VerifyCRC32 checks if data matches expected checksum
func VerifyCRC32(data []byte, expected uint32) bool {
	return CRC32(data) == expected
}
