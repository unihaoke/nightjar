package utils

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims 是平台 JWT 声明。
type Claims struct {
	UserID   int64  `json:"uid"`
	Username string `json:"uname"`
	RoleCode string `json:"role"`
	// EnvScope / GroupScope 固化进 Token，服务端仍以数据库为准做二次校验。
	EnvScope   []string `json:"env_scope,omitempty"`
	GroupScope []string `json:"group_scope,omitempty"`
	jwt.RegisteredClaims
}

// TokenManager 负责签发与解析 JWT。
type TokenManager struct {
	secret    []byte
	issuer    string
	accessTTL time.Duration
}

// NewTokenManager 构造 TokenManager。
func NewTokenManager(secret, issuer string, accessTTL time.Duration) (*TokenManager, error) {
	if len(secret) == 0 {
		return nil, errors.New("jwt secret is empty")
	}
	if accessTTL <= 0 {
		accessTTL = 8 * time.Hour
	}
	return &TokenManager{secret: []byte(secret), issuer: issuer, accessTTL: accessTTL}, nil
}

// TTL 返回访问令牌有效期。
func (m *TokenManager) TTL() time.Duration { return m.accessTTL }

// Issue 签发访问令牌。
func (m *TokenManager) Issue(userID int64, username, roleCode string, envScope, groupScope []string) (string, time.Time, error) {
	now := time.Now()
	expires := now.Add(m.accessTTL)
	claims := Claims{
		UserID:     userID,
		Username:   username,
		RoleCode:   roleCode,
		EnvScope:   envScope,
		GroupScope: groupScope,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, expires, nil
}

// Parse 解析并校验令牌。
func (m *TokenManager) Parse(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithIssuer(m.issuer), jwt.WithLeeway(30*time.Second))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	if !token.Valid {
		return nil, ErrTokenInvalid
	}
	return claims, nil
}

// 认证错误，供上层映射到 apperr。
var (
	// ErrTokenExpired 表示令牌过期。
	ErrTokenExpired = errors.New("token expired")
	// ErrTokenInvalid 表示令牌无效。
	ErrTokenInvalid = errors.New("token invalid")
)
