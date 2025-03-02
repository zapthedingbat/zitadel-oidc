package rp

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gopkg.in/square/go-jose.v2"

	"github.com/zitadel/oidc/pkg/client"
	httphelper "github.com/zitadel/oidc/pkg/http"
	"github.com/zitadel/oidc/pkg/oidc"
)

const (
	idTokenKey = "id_token"
	stateParam = "state"
	pkceCode   = "pkce"
)

var (
	ErrUserInfoSubNotMatching = errors.New("sub from userinfo does not match the sub from the id_token")
)

//RelyingParty declares the minimal interface for oidc clients
type RelyingParty interface {
	//OAuthConfig returns the oauth2 Config
	OAuthConfig() *oauth2.Config

	//Issuer returns the issuer of the oidc config
	Issuer() string

	//IsPKCE returns if authorization is done using `Authorization Code Flow with Proof Key for Code Exchange (PKCE)`
	IsPKCE() bool

	//CookieHandler returns a http cookie handler used for various state transfer cookies
	CookieHandler() *httphelper.CookieHandler

	//HttpClient returns a http client used for calls to the openid provider, e.g. calling token endpoint
	HttpClient() *http.Client

	//IsOAuth2Only specifies whether relaying party handles only oauth2 or oidc calls
	IsOAuth2Only() bool

	//Signer is used if the relaying party uses the JWT Profile
	Signer() jose.Signer

	//UserinfoEndpoint returns the userinfo
	UserinfoEndpoint() string

	//IDTokenVerifier returns the verifier interface used for oidc id_token verification
	IDTokenVerifier() IDTokenVerifier
	//ErrorHandler returns the handler used for callback errors

	ErrorHandler() func(http.ResponseWriter, *http.Request, string, string, string)
}

type ErrorHandler func(w http.ResponseWriter, r *http.Request, errorType string, errorDesc string, state string)

var (
	DefaultErrorHandler ErrorHandler = func(w http.ResponseWriter, r *http.Request, errorType string, errorDesc string, state string) {
		http.Error(w, errorType+": "+errorDesc, http.StatusInternalServerError)
	}
)

type relyingParty struct {
	issuer            string
	DiscoveryEndpoint string
	endpoints         Endpoints
	oauthConfig       *oauth2.Config
	oauth2Only        bool
	pkce              bool

	httpClient    *http.Client
	cookieHandler *httphelper.CookieHandler

	errorHandler    func(http.ResponseWriter, *http.Request, string, string, string)
	idTokenVerifier IDTokenVerifier
	verifierOpts    []VerifierOption
	signer          jose.Signer
}

func (rp *relyingParty) OAuthConfig() *oauth2.Config {
	return rp.oauthConfig
}

func (rp *relyingParty) Issuer() string {
	return rp.issuer
}

func (rp *relyingParty) IsPKCE() bool {
	return rp.pkce
}

func (rp *relyingParty) CookieHandler() *httphelper.CookieHandler {
	return rp.cookieHandler
}

func (rp *relyingParty) HttpClient() *http.Client {
	return rp.httpClient
}

func (rp *relyingParty) IsOAuth2Only() bool {
	return rp.oauth2Only
}

func (rp *relyingParty) Signer() jose.Signer {
	return rp.signer
}

func (rp *relyingParty) UserinfoEndpoint() string {
	return rp.endpoints.UserinfoURL
}

func (rp *relyingParty) IDTokenVerifier() IDTokenVerifier {
	if rp.idTokenVerifier == nil {
		rp.idTokenVerifier = NewIDTokenVerifier(rp.issuer, rp.oauthConfig.ClientID, NewRemoteKeySet(rp.httpClient, rp.endpoints.JKWsURL), rp.verifierOpts...)
	}
	return rp.idTokenVerifier
}

func (rp *relyingParty) ErrorHandler() func(http.ResponseWriter, *http.Request, string, string, string) {
	if rp.errorHandler == nil {
		rp.errorHandler = DefaultErrorHandler
	}
	return rp.errorHandler
}

//NewRelyingPartyOAuth creates an (OAuth2) RelyingParty with the given
//OAuth2 Config and possible configOptions
//it will use the AuthURL and TokenURL set in config
func NewRelyingPartyOAuth(config *oauth2.Config, options ...Option) (RelyingParty, error) {
	rp := &relyingParty{
		oauthConfig: config,
		httpClient:  httphelper.DefaultHTTPClient,
		oauth2Only:  true,
	}

	for _, optFunc := range options {
		if err := optFunc(rp); err != nil {
			return nil, err
		}
	}

	return rp, nil
}

//NewRelyingPartyOIDC creates an (OIDC) RelyingParty with the given
//issuer, clientID, clientSecret, redirectURI, scopes and possible configOptions
//it will run discovery on the provided issuer and use the found endpoints
func NewRelyingPartyOIDC(issuer, clientID, clientSecret, redirectURI string, scopes []string, options ...Option) (RelyingParty, error) {
	rp := &relyingParty{
		issuer: issuer,
		oauthConfig: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURI,
			Scopes:       scopes,
		},
		httpClient: httphelper.DefaultHTTPClient,
		oauth2Only: false,
	}

	for _, optFunc := range options {
		if err := optFunc(rp); err != nil {
			return nil, err
		}
	}
	discoveryConfiguration, err := client.Discover(rp.issuer, rp.httpClient, rp.DiscoveryEndpoint)
	if err != nil {
		return nil, err
	}
	endpoints := GetEndpoints(discoveryConfiguration)
	rp.oauthConfig.Endpoint = endpoints.Endpoint
	rp.endpoints = endpoints

	return rp, nil
}

//Option is the type for providing dynamic options to the relyingParty
type Option func(*relyingParty) error

func WithCustomDiscoveryUrl(url string) Option {
	return func(rp *relyingParty) error {
		rp.DiscoveryEndpoint = url
		return nil
	}
}

//WithCookieHandler set a `CookieHandler` for securing the various redirects
func WithCookieHandler(cookieHandler *httphelper.CookieHandler) Option {
	return func(rp *relyingParty) error {
		rp.cookieHandler = cookieHandler
		return nil
	}
}

//WithPKCE sets the RP to use PKCE (oauth2 code challenge)
//it also sets a `CookieHandler` for securing the various redirects
//and exchanging the code challenge
func WithPKCE(cookieHandler *httphelper.CookieHandler) Option {
	return func(rp *relyingParty) error {
		rp.pkce = true
		rp.cookieHandler = cookieHandler
		return nil
	}
}

//WithHTTPClient provides the ability to set an http client to be used for the relaying party and verifier
func WithHTTPClient(client *http.Client) Option {
	return func(rp *relyingParty) error {
		rp.httpClient = client
		return nil
	}
}

func WithErrorHandler(errorHandler ErrorHandler) Option {
	return func(rp *relyingParty) error {
		rp.errorHandler = errorHandler
		return nil
	}
}

func WithVerifierOpts(opts ...VerifierOption) Option {
	return func(rp *relyingParty) error {
		rp.verifierOpts = opts
		return nil
	}
}

// WithClientKey specifies the path to the key.json to be used for the JWT Profile Client Authentication on the token endpoint
//
//deprecated: use WithJWTProfile(SignerFromKeyPath(path)) instead
func WithClientKey(path string) Option {
	return WithJWTProfile(SignerFromKeyPath(path))
}

// WithJWTProfile creates a signer used for the JWT Profile Client Authentication on the token endpoint
func WithJWTProfile(signerFromKey SignerFromKey) Option {
	return func(rp *relyingParty) error {
		signer, err := signerFromKey()
		if err != nil {
			return err
		}
		rp.signer = signer
		return nil
	}
}

type SignerFromKey func() (jose.Signer, error)

func SignerFromKeyPath(path string) SignerFromKey {
	return func() (jose.Signer, error) {
		config, err := client.ConfigFromKeyFile(path)
		if err != nil {
			return nil, err
		}
		return client.NewSignerFromPrivateKeyByte([]byte(config.Key), config.KeyID)
	}
}

func SignerFromKeyFile(fileData []byte) SignerFromKey {
	return func() (jose.Signer, error) {
		config, err := client.ConfigFromKeyFileData(fileData)
		if err != nil {
			return nil, err
		}
		return client.NewSignerFromPrivateKeyByte([]byte(config.Key), config.KeyID)
	}
}

func SignerFromKeyAndKeyID(key []byte, keyID string) SignerFromKey {
	return func() (jose.Signer, error) {
		return client.NewSignerFromPrivateKeyByte(key, keyID)
	}
}

//Discover calls the discovery endpoint of the provided issuer and returns the found endpoints
//
//deprecated: use client.Discover
func Discover(issuer string, httpClient *http.Client) (Endpoints, error) {
	wellKnown := strings.TrimSuffix(issuer, "/") + oidc.DiscoveryEndpoint
	req, err := http.NewRequest("GET", wellKnown, nil)
	if err != nil {
		return Endpoints{}, err
	}
	discoveryConfig := new(oidc.DiscoveryConfiguration)
	err = httphelper.HttpRequest(httpClient, req, &discoveryConfig)
	if err != nil {
		return Endpoints{}, err
	}
	if discoveryConfig.Issuer != issuer {
		return Endpoints{}, oidc.ErrIssuerInvalid
	}
	return GetEndpoints(discoveryConfig), nil
}

//AuthURL returns the auth request url
//(wrapping the oauth2 `AuthCodeURL`)
func AuthURL(state string, rp RelyingParty, opts ...AuthURLOpt) string {
	authOpts := make([]oauth2.AuthCodeOption, 0)
	for _, opt := range opts {
		authOpts = append(authOpts, opt()...)
	}
	return rp.OAuthConfig().AuthCodeURL(state, authOpts...)
}

//AuthURLHandler extends the `AuthURL` method with a http redirect handler
//including handling setting cookie for secure `state` transfer
func AuthURLHandler(stateFn func() string, rp RelyingParty) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts := make([]AuthURLOpt, 0)
		state := stateFn()
		if err := trySetStateCookie(w, state, rp); err != nil {
			http.Error(w, "failed to create state cookie: "+err.Error(), http.StatusUnauthorized)
			return
		}
		if rp.IsPKCE() {
			codeChallenge, err := GenerateAndStoreCodeChallenge(w, rp)
			if err != nil {
				http.Error(w, "failed to create code challenge: "+err.Error(), http.StatusUnauthorized)
				return
			}
			opts = append(opts, WithCodeChallenge(codeChallenge))
		}
		http.Redirect(w, r, AuthURL(state, rp, opts...), http.StatusFound)
	}
}

//GenerateAndStoreCodeChallenge generates a PKCE code challenge and stores its verifier into a secure cookie
func GenerateAndStoreCodeChallenge(w http.ResponseWriter, rp RelyingParty) (string, error) {
	codeVerifier := base64.RawURLEncoding.EncodeToString([]byte(uuid.New().String()))
	if err := rp.CookieHandler().SetCookie(w, pkceCode, codeVerifier); err != nil {
		return "", err
	}
	return oidc.NewSHACodeChallenge(codeVerifier), nil
}

//CodeExchange handles the oauth2 code exchange, extracting and validating the id_token
//returning it parsed together with the oauth2 tokens (access, refresh)
func CodeExchange(ctx context.Context, code string, rp RelyingParty, opts ...CodeExchangeOpt) (tokens *oidc.Tokens, err error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, rp.HttpClient())
	codeOpts := make([]oauth2.AuthCodeOption, 0)
	for _, opt := range opts {
		codeOpts = append(codeOpts, opt()...)
	}

	token, err := rp.OAuthConfig().Exchange(ctx, code, codeOpts...)
	if err != nil {
		return nil, err
	}

	if rp.IsOAuth2Only() {
		return &oidc.Tokens{Token: token}, nil
	}

	idTokenString, ok := token.Extra(idTokenKey).(string)
	if !ok {
		return nil, errors.New("id_token missing")
	}

	idToken, err := VerifyTokens(ctx, token.AccessToken, idTokenString, rp.IDTokenVerifier())
	if err != nil {
		return nil, err
	}

	return &oidc.Tokens{Token: token, IDTokenClaims: idToken, IDToken: idTokenString}, nil
}

type CodeExchangeCallback func(w http.ResponseWriter, r *http.Request, tokens *oidc.Tokens, state string, rp RelyingParty)

//CodeExchangeHandler extends the `CodeExchange` method with a http handler
//including cookie handling for secure `state` transfer
//and optional PKCE code verifier checking
func CodeExchangeHandler(callback CodeExchangeCallback, rp RelyingParty) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := tryReadStateCookie(w, r, rp)
		if err != nil {
			http.Error(w, "failed to get state: "+err.Error(), http.StatusUnauthorized)
			return
		}
		params := r.URL.Query()
		if params.Get("error") != "" {
			rp.ErrorHandler()(w, r, params.Get("error"), params.Get("error_description"), state)
			return
		}
		codeOpts := make([]CodeExchangeOpt, 0)
		if rp.IsPKCE() {
			codeVerifier, err := rp.CookieHandler().CheckCookie(r, pkceCode)
			if err != nil {
				http.Error(w, "failed to get code verifier: "+err.Error(), http.StatusUnauthorized)
				return
			}
			codeOpts = append(codeOpts, WithCodeVerifier(codeVerifier))
		}
		if rp.Signer() != nil {
			assertion, err := client.SignedJWTProfileAssertion(rp.OAuthConfig().ClientID, []string{rp.Issuer()}, time.Hour, rp.Signer())
			if err != nil {
				http.Error(w, "failed to build assertion: "+err.Error(), http.StatusUnauthorized)
				return
			}
			codeOpts = append(codeOpts, WithClientAssertionJWT(assertion))
		}
		tokens, err := CodeExchange(r.Context(), params.Get("code"), rp, codeOpts...)
		if err != nil {
			http.Error(w, "failed to exchange token: "+err.Error(), http.StatusUnauthorized)
			return
		}
		callback(w, r, tokens, state, rp)
	}
}

type CodeExchangeUserinfoCallback func(w http.ResponseWriter, r *http.Request, tokens *oidc.Tokens, state string, provider RelyingParty, info oidc.UserInfo)

//UserinfoCallback wraps the callback function of the CodeExchangeHandler
//and calls the userinfo endpoint with the access token
//on success it will pass the userinfo into its callback function as well
func UserinfoCallback(f CodeExchangeUserinfoCallback) CodeExchangeCallback {
	return func(w http.ResponseWriter, r *http.Request, tokens *oidc.Tokens, state string, rp RelyingParty) {
		info, err := Userinfo(tokens.AccessToken, tokens.TokenType, tokens.IDTokenClaims.GetSubject(), rp)
		if err != nil {
			http.Error(w, "userinfo failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
		f(w, r, tokens, state, rp, info)
	}
}

//Userinfo will call the OIDC Userinfo Endpoint with the provided token
func Userinfo(token, tokenType, subject string, rp RelyingParty) (oidc.UserInfo, error) {
	req, err := http.NewRequest("GET", rp.UserinfoEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", tokenType+" "+token)
	userinfo := oidc.NewUserInfo()
	if err := httphelper.HttpRequest(rp.HttpClient(), req, &userinfo); err != nil {
		return nil, err
	}
	if userinfo.GetSubject() != subject {
		return nil, ErrUserInfoSubNotMatching
	}
	return userinfo, nil
}

func trySetStateCookie(w http.ResponseWriter, state string, rp RelyingParty) error {
	if rp.CookieHandler() != nil {
		if err := rp.CookieHandler().SetCookie(w, stateParam, state); err != nil {
			return err
		}
	}
	return nil
}

func tryReadStateCookie(w http.ResponseWriter, r *http.Request, rp RelyingParty) (state string, err error) {
	if rp.CookieHandler() == nil {
		return r.FormValue(stateParam), nil
	}
	state, err = rp.CookieHandler().CheckQueryCookie(r, stateParam)
	if err != nil {
		return "", err
	}
	rp.CookieHandler().DeleteCookie(w, stateParam)
	return state, nil
}

type OptionFunc func(RelyingParty)

type Endpoints struct {
	oauth2.Endpoint
	IntrospectURL string
	UserinfoURL   string
	JKWsURL       string
}

func GetEndpoints(discoveryConfig *oidc.DiscoveryConfiguration) Endpoints {
	return Endpoints{
		Endpoint: oauth2.Endpoint{
			AuthURL:   discoveryConfig.AuthorizationEndpoint,
			AuthStyle: oauth2.AuthStyleAutoDetect,
			TokenURL:  discoveryConfig.TokenEndpoint,
		},
		IntrospectURL: discoveryConfig.IntrospectionEndpoint,
		UserinfoURL:   discoveryConfig.UserinfoEndpoint,
		JKWsURL:       discoveryConfig.JwksURI,
	}
}

type AuthURLOpt func() []oauth2.AuthCodeOption

//WithCodeChallenge sets the `code_challenge` params in the auth request
func WithCodeChallenge(codeChallenge string) AuthURLOpt {
	return func() []oauth2.AuthCodeOption {
		return []oauth2.AuthCodeOption{
			oauth2.SetAuthURLParam("code_challenge", codeChallenge),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		}
	}
}

//WithPrompt sets the `prompt` params in the auth request
func WithPrompt(prompt ...string) AuthURLOpt {
	return func() []oauth2.AuthCodeOption {
		return []oauth2.AuthCodeOption{
			oauth2.SetAuthURLParam("prompt", oidc.SpaceDelimitedArray(prompt).Encode()),
		}
	}
}

type CodeExchangeOpt func() []oauth2.AuthCodeOption

//WithCodeVerifier sets the `code_verifier` param in the token request
func WithCodeVerifier(codeVerifier string) CodeExchangeOpt {
	return func() []oauth2.AuthCodeOption {
		return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("code_verifier", codeVerifier)}
	}
}

//WithClientAssertionJWT sets the `client_assertion` param in the token request
func WithClientAssertionJWT(clientAssertion string) CodeExchangeOpt {
	return func() []oauth2.AuthCodeOption {
		return client.ClientAssertionCodeOptions(clientAssertion)
	}
}
