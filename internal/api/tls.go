package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// GenerateSelfSignedCert генерирует self-signed SSL сертификат и ключ
func GenerateSelfSignedCert(certFile, keyFile string) error {
	// Создание приватного ключа RSA
	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	// Шаблон сертификата
	notBefore := time.Now()
	notAfter := notBefore.Add(365 * 24 * time.Hour) // 1 год

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Ollama Load Balancer"},
			CommonName:   "localhost",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", "ollama-legion"},
		IPAddresses:           nil, // будет добавлено ниже
	}

	// Добавляем IP адреса
	template.IPAddresses = append(template.IPAddresses, []byte{127, 0, 0, 1})
	template.IPAddresses = append(template.IPAddresses, []byte{0, 0, 0, 0})

	// Создание сертификата
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	// Создание директории для сертификатов если не существует
	certDir := filepath.Dir(certFile)
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return fmt.Errorf("failed to create certificate directory: %w", err)
	}

	// Запись сертификата
	certOut, err := os.Create(certFile)
	if err != nil {
		return fmt.Errorf("failed to create certificate file: %w", err)
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("failed to write certificate: %w", err)
	}

	// Запись приватного ключа
	keyOut, err := os.Create(keyFile)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer keyOut.Close()

	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}); err != nil {
		return fmt.Errorf("failed to write private key: %w", err)
	}

	// Установка правильных прав доступа
	if err := os.Chmod(certFile, 0644); err != nil {
		return fmt.Errorf("failed to set certificate permissions: %w", err)
	}
	if err := os.Chmod(keyFile, 0600); err != nil {
		return fmt.Errorf("failed to set key permissions: %w", err)
	}

	return nil
}

// LoadTLSConfig загружает TLS конфигурацию из файлов сертификатов
func LoadTLSConfig(config *types.TLSConfig) (*tls.Config, error) {
	if err := ValidateTLSConfig(config); err != nil {
		return nil, err
	}

	// Загрузка сертификата
	cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
	}

	// Определение минимальной версии TLS
	minVersion := tls.VersionTLS12
	switch config.MinVersion {
	case "TLS13":
		minVersion = tls.VersionTLS13
	case "TLS12", "":
		minVersion = tls.VersionTLS12
	default:
		return nil, fmt.Errorf("unsupported TLS version: %s", config.MinVersion)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   uint16(minVersion),
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}

	return tlsConfig, nil
}

// ValidateTLSConfig проверяет корректность TLS конфигурации
func ValidateTLSConfig(config *types.TLSConfig) error {
	if !config.Enabled {
		return nil
	}

	if config.CertFile == "" {
		return fmt.Errorf("TLS certificate file is not specified")
	}

	if config.KeyFile == "" {
		return fmt.Errorf("TLS key file is not specified")
	}

	// Проверка существования файлов если AutoCert не включен
	if !config.AutoCert {
		if _, err := os.Stat(config.CertFile); os.IsNotExist(err) {
			return fmt.Errorf("TLS certificate file does not exist: %s", config.CertFile)
		}
		if _, err := os.Stat(config.KeyFile); os.IsNotExist(err) {
			return fmt.Errorf("TLS key file does not exist: %s", config.KeyFile)
		}
	}

	// Проверка версии TLS
	switch config.MinVersion {
	case "TLS12", "TLS13", "":
		// допустимые значения
	default:
		return fmt.Errorf("unsupported TLS version: %s (supported: TLS12, TLS13)", config.MinVersion)
	}

	return nil
}

// HTTPSRedirectMiddleware создает middleware для редиректа HTTP -> HTTPS
func HTTPSRedirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Проверка, является ли соединение уже HTTPS
		if r.TLS != nil {
			next.ServeHTTP(w, r)
			return
		}

		// Проверка заголовка X-Forwarded-Proto (для работы за reverse proxy)
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
			next.ServeHTTP(w, r)
			return
		}

		// Редирект на HTTPS
		host := r.Host
		if colonIndex := findColon(host); colonIndex != -1 {
			host = host[:colonIndex]
		}
		
		url := "https://" + host + r.URL.Path
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}
		
		http.Redirect(w, r, url, http.StatusMovedPermanently)
	})
}

// findColon находит позицию двоеточия в строке
func findColon(s string) int {
	for i, c := range s {
		if c == ':' {
			return i
		}
	}
	return -1
}

// EnsureTLSCertificates проверяет наличие сертификатов и генерирует self-signed при необходимости
func EnsureTLSCertificates(config *types.TLSConfig) error {
	if !config.Enabled {
		return nil
	}

	if config.AutoCert {
		// Проверка существования сертификатов
		if _, err := os.Stat(config.CertFile); os.IsNotExist(err) {
			fmt.Printf("[TLS] Generating self-signed certificate: %s\n", config.CertFile)
			if err := GenerateSelfSignedCert(config.CertFile, config.KeyFile); err != nil {
				return fmt.Errorf("failed to generate self-signed certificate: %w", err)
			}
			fmt.Printf("[TLS] Self-signed certificate generated successfully\n")
		}
	}

	return ValidateTLSConfig(config)
}
