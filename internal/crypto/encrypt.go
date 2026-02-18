package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// SecureHeader: Metadados criptografados
// Magic (4) + TamanhoOriginal (8) + HMAC (32) + Reservado (4) = 48 bytes
const SecureHeaderSize = 48

type SecureHeader struct {
	Magic        [4]byte  // Identificador "NCC2"
	OriginalSize uint64   // Tamanho original
	ContentHMAC  [32]byte // HMAC-SHA256 do plaintext
	Reserved     [4]byte  // Padding
}

// Encode serializa SecureHeader
func (sh SecureHeader) Encode() []byte {
	buf := make([]byte, SecureHeaderSize)
	copy(buf[0:4], sh.Magic[:])
	binary.BigEndian.PutUint64(buf[4:12], sh.OriginalSize)
	copy(buf[12:44], sh.ContentHMAC[:])
	copy(buf[44:48], sh.Reserved[:])
	return buf
}

// Decode lê dados em SecureHeader
func DecodeSecureHeader(data []byte) (SecureHeader, error) {
	var sh SecureHeader
	if len(data) < SecureHeaderSize {
		return sh, io.ErrUnexpectedEOF
	}
	copy(sh.Magic[:], data[0:4])
	sh.OriginalSize = binary.BigEndian.Uint64(data[4:12])
	copy(sh.ContentHMAC[:], data[12:44])
	copy(sh.Reserved[:], data[44:48])
	return sh, nil
}

// EncryptWithHash: Criptografa dados protegendo HMAC e tamanho no header
func EncryptWithHash(plaintext []byte, password string) ([]byte, error) {
	// Gerar salt
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}

	// Derivação de chaves (Argon2id): 32 enc + 32 hmac
	// Params: 6 iterações, 128MB memória
	keyMaterial := argon2.IDKey([]byte(password), salt, 6, 128*1024, 4, 64)
	encKey := keyMaterial[:32]
	hmacKey := keyMaterial[32:]

	// Calcular HMAC dos dados originais
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(plaintext)
	hmacBytes := mac.Sum(nil)

	// Criar header seguro
	var hmacArr [32]byte
	copy(hmacArr[:], hmacBytes)

	secureHeader := SecureHeader{
		Magic:        [4]byte{'N', 'C', 'C', '2'},
		OriginalSize: uint64(len(plaintext)),
		ContentHMAC:  hmacArr,
	}

	// Adicionar header aos dados
	headerBytes := secureHeader.Encode()
	plaintextWithHeader := append(headerBytes, plaintext...)

	// Criptografar dados combinados
	aead, err := chacha20poly1305.New(encKey)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := aead.Seal(nonce, nonce, plaintextWithHeader, nil)
	return append(salt, ciphertext...), nil
}

// DecryptWithHash: Descriptografa e verifica integridade (HMAC)
func DecryptWithHash(ciphertext []byte, password string) ([]byte, error) {
	if len(ciphertext) < 16 {
		return nil, errors.New("failed to decrypt: invalid data")
	}

	salt := ciphertext[:16]
	ciphertext = ciphertext[16:]

	// Derivar chaves
	// Segurança: Mesmos parâmetros
	keyMaterial := argon2.IDKey([]byte(password), salt, 6, 128*1024, 4, 64)
	encKey := keyMaterial[:32]
	hmacKey := keyMaterial[32:]

	aead, err := chacha20poly1305.New(encKey)
	if err != nil {
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	nonce, ciphertext := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plaintextWithHeader, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Erro genérico (evita side-channels)
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	// Extrair header
	if len(plaintextWithHeader) < SecureHeaderSize {
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	secureHeader, err := DecodeSecureHeader(plaintextWithHeader[:SecureHeaderSize])
	if err != nil {
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	// Verificar magic
	if secureHeader.Magic != [4]byte{'N', 'C', 'C', '2'} {
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	// Verificar tamanho
	plaintext := plaintextWithHeader[SecureHeaderSize:]
	if uint64(len(plaintext)) != secureHeader.OriginalSize {
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	// Verificar HMAC
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(plaintext)
	computedHMAC := mac.Sum(nil)

	if subtle.ConstantTimeCompare(computedHMAC, secureHeader.ContentHMAC[:]) != 1 {
		return nil, errors.New("failed to decrypt: invalid password or corrupted data")
	}

	return plaintext, nil
}
