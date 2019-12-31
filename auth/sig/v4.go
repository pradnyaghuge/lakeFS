package sig

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	v4authHeaderName   = "Authorization"
	v4authHeaderPrefix = "AWS4-HMAC-SHA256"
	v4scopeTerminator  = "aws4_request"
	v4timeFormat       = "20060102T150405Z"
	v4shortTimeFormat  = "20060102"
)

var (
	// AWS4-HMAC-SHA256 Credential=AKIAJKKRUNSYM5MZSHDQ/20191031/us-east-1/s3/aws4_request,SignedHeaders=date;host;x-amz-content-sha256;x-amz-date,Signature=54ff856d2908e13d3bee41d1f78f808068d0ee27a9af212561dc0ea56e8cf633
	V4AuthHeaderRegexp      = regexp.MustCompile(`AWS4-HMAC-SHA256 Credential=(?P<AccessKeyId>[A-Z0-9]{20})/(?P<Date>\d{8})/(?P<Region>[\w\-]+)/(?P<Service>[\w\-]+)/aws4_request,\s*SignedHeaders=(?P<SignatureHeaders>[\w\-\;]+),\s*Signature=(?P<Signature>[abcdef0123456789]{64})`)
	V4CredentialScopeRegexp = regexp.MustCompile(`(?P<AccessKeyId>[A-Z0-9]{20})/(?P<Date>\d{8})/(?P<Region>[\w\-]+)/(?P<Service>[\w\-]+)/aws4_request`)
)

// Authorization:
// 		AWS4-HMAC-SHA256
// 		Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request,
// 		SignedHeaders=content-type;host;x-amz-date,
// 		Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7

type V4Auth struct {
	AccessKeyId         string
	Date                string
	Region              string
	Service             string
	SignedHeaders       []string
	SignedHeadersString string
	Signature           string
}

func (a V4Auth) GetAccessKeyId() string {
	return a.AccessKeyId
}

func splitHeaders(headers string) ([]string, error) {
	headerValues := strings.Split(headers, ";")
	sort.Strings(headerValues)
	return headerValues, nil
}

func ParseV4AuthContext(r *http.Request) (V4Auth, error) {
	var ctx V4Auth

	// start by trying to extract the data from the Authorization header
	headerValue := r.Header.Get(v4authHeaderName)
	if len(headerValue) > 0 {
		match := V4AuthHeaderRegexp.FindStringSubmatch(headerValue)
		if len(match) == 0 {
			return ctx, ErrHeaderMalformed
		}
		result := make(map[string]string)
		for i, name := range V4AuthHeaderRegexp.SubexpNames() {
			if i != 0 && name != "" {
				result[name] = match[i]
			}
		}
		headers, err := splitHeaders(result["SignatureHeaders"])
		if err != nil {
			return ctx, err
		}
		ctx.AccessKeyId = result["AccessKeyId"]
		ctx.Date = result["Date"]
		ctx.Region = result["Region"]
		ctx.Service = result["Service"]

		ctx.Signature = result["Signature"]

		ctx.SignedHeaders = headers
		ctx.SignedHeadersString = result["SignatureHeaders"]
		return ctx, nil
	}

	// otherwise, see if we have all the required query parameters
	query := r.URL.Query()
	algorithm := query.Get("X-Amz-Algorithm")
	if len(algorithm) == 0 || !strings.EqualFold(algorithm, v4authHeaderPrefix) {
		return ctx, ErrMissingAuthData
	}
	credentialScope := query.Get("X-Amz-Credential")
	if len(credentialScope) == 0 {
		return ctx, ErrMissingAuthData
	}
	credsMatch := V4CredentialScopeRegexp.FindStringSubmatch(credentialScope)
	if len(credsMatch) == 0 {
		return ctx, ErrHeaderMalformed
	}
	credsResult := make(map[string]string)
	for i, name := range V4CredentialScopeRegexp.SubexpNames() {
		if i != 0 && name != "" {
			credsResult[name] = credsMatch[i]
		}
	}
	ctx.AccessKeyId = credsResult["AccessKeyId"]
	ctx.Date = credsResult["Date"]
	ctx.Region = credsResult["Region"]
	ctx.Service = credsResult["Service"]

	ctx.SignedHeadersString = query.Get("X-Amz-SignedHeaders")
	headers, err := splitHeaders(ctx.SignedHeadersString)
	if err != nil {
		return ctx, err
	}
	ctx.SignedHeaders = headers
	ctx.Signature = query.Get("X-Amz-Signature=")
	return ctx, nil
}

func V4Verify(auth V4Auth, credentials Credentials, r *http.Request) error {
	// copy body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}
	// reset body
	r.Body = ioutil.NopCloser(bytes.NewReader(body))

	ctx := &verificationCtx{
		Request:   r,
		Body:      body,
		Query:     r.URL.Query(),
		AuthValue: auth,
	}

	canonicalRequest := ctx.buildCanonicalRequest()
	stringToSign, err := ctx.buildSignedString(canonicalRequest)
	if err != nil {
		return err
	}
	// sign
	signingKey := ctx.createSignature(credentials.GetAccessSecretKey(), auth.Date, auth.Region, auth.Service)
	signature := hex.EncodeToString(ctx.sign(signingKey, stringToSign))

	// compare signatures
	if !strings.EqualFold(signature, auth.Signature) {
		return ErrBadSignature
	}
	return nil
}

type verificationCtx struct {
	Request   *http.Request
	Body      []byte
	Query     url.Values
	AuthValue V4Auth
}

func (ctx *verificationCtx) queryEscape(str string) string {
	return strings.ReplaceAll(url.QueryEscape(str), "+", "%20")
}

func (ctx *verificationCtx) canonicalizeQueryString() string {
	queryNames := make([]string, len(ctx.Query))
	index := 0
	for k := range ctx.Query {
		queryNames[index] = k
		index++
	}
	sort.Strings(queryNames)
	buf := make([]string, len(queryNames))
	for i, key := range queryNames {
		buf[i] = fmt.Sprintf("%s=%s", ctx.queryEscape(key), ctx.queryEscape(ctx.Query.Get(key)))
	}
	return strings.Join(buf, "&")
}

func (ctx *verificationCtx) canonicalizeHeaders(headers []string) string {
	var buf strings.Builder
	for _, header := range headers {
		var value string
		if strings.EqualFold(strings.ToLower(header), "host") {
			// in Go, Host is removed from the headers and is promoted to request.Host for some reason
			value = ctx.Request.Host
		} else {
			value = ctx.Request.Header.Get(header)
		}
		buf.WriteString(header)
		buf.WriteString(":")
		buf.WriteString(ctx.trimAll(value))
		buf.WriteString("\n")
	}
	return buf.String()
}
func (ctx *verificationCtx) trimAll(str string) string {
	str = strings.TrimSpace(str)
	inSpace := false
	var buf strings.Builder
	for _, ch := range str {
		if unicode.IsSpace(ch) && !inSpace {
			// first space to appear
			buf.WriteRune(ch)
			inSpace = true
		} else if unicode.IsSpace(ch) && inSpace {
			// consecutive space
			continue
		} else {
			// not a space
			buf.WriteRune(ch)
			inSpace = false
		}
	}
	return buf.String()
}

func (ctx *verificationCtx) payloadHash() string {
	body := ctx.Body
	if body == nil {
		body = []byte{}
	}
	h := sha256.New()
	h.Write(body)
	hashedBody := h.Sum(nil)
	return hex.EncodeToString(hashedBody)
}

func (ctx *verificationCtx) buildCanonicalRequest() string {
	// Step 1: Canonical request
	method := ctx.Request.Method
	canonicalUri := ctx.Request.URL.Path
	canonicalQueryString := ctx.canonicalizeQueryString()
	canonicalHeaders := ctx.canonicalizeHeaders(ctx.AuthValue.SignedHeaders)
	signedHeaders := ctx.AuthValue.SignedHeadersString
	payloadHash := ctx.payloadHash()
	canonicalRequest := strings.Join([]string{
		method,
		canonicalUri,
		canonicalQueryString,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	return canonicalRequest
}

func (ctx *verificationCtx) getAmzDate() (string, error) {
	// https://docs.aws.amazon.com/general/latest/gr/sigv4-date-handling.html
	amzDate := ctx.Request.URL.Query().Get("x-amz-date")
	if len(amzDate) == 0 {
		amzDate = ctx.Request.Header.Get("x-amz-date")
		if len(amzDate) == 0 {
			amzDate = ctx.Request.Header.Get("date")
			if len(amzDate) == 0 {
				return "", ErrMissingDateHeader
			}
		}
	}

	// parse date
	ts, err := time.Parse(v4timeFormat, amzDate)
	if err != nil {
		return "", ErrDateHeaderMalformed
	}

	// parse signature date
	sigTs, err := time.Parse(v4shortTimeFormat, ctx.AuthValue.Date)
	if err != nil {
		return "", ErrSignatureDateMalformed
	}

	// ensure same date
	if sigTs.Year() != ts.Year() || sigTs.Month() != ts.Month() || sigTs.Day() != ts.Day() {
		return "", ErrSignatureDateMalformed
	}

	return amzDate, nil
}

func (ctx *verificationCtx) sign(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func (ctx *verificationCtx) createSignature(key, dateStamp, region, service string) []byte {
	kDate := ctx.sign([]byte(fmt.Sprintf("AWS4%s", key)), dateStamp)
	kRegion := ctx.sign(kDate, region)
	kService := ctx.sign(kRegion, service)
	kSigning := ctx.sign(kService, v4scopeTerminator)
	return kSigning
}

func (ctx *verificationCtx) buildSignedString(canonicalRequest string) (string, error) {
	// Step 2: Create string to sign
	algorithm := v4authHeaderPrefix
	credentialScope := strings.Join([]string{
		ctx.AuthValue.Date,
		ctx.AuthValue.Region,
		ctx.AuthValue.Service,
		v4scopeTerminator,
	}, "/")
	amzDate, err := ctx.getAmzDate()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(canonicalRequest))
	hashedCanonicalRequest := hex.EncodeToString(h.Sum(nil))
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		credentialScope,
		hashedCanonicalRequest,
	}, "\n")
	return stringToSign, nil
}

type V4Authenticator struct {
	request *http.Request
	ctx     V4Auth
}

func (a *V4Authenticator) Parse() (SigContext, error) {
	var ctx V4Auth
	var err error
	ctx, err = ParseV4AuthContext(a.request)
	if err != nil {
		return ctx, err
	}
	a.ctx = ctx
	return a.ctx, nil
}

func (a *V4Authenticator) String() string {
	return "sigv4"
}

func (a *V4Authenticator) Verify(creds Credentials) error {
	return V4Verify(a.ctx, creds, a.request)
}

func NewV4Authenticatior(r *http.Request) SigAuthenticator {
	return &V4Authenticator{
		request: r,
		ctx:     V4Auth{},
	}
}