package aws_helper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/gruntwork-io/go-commons/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateAwsEnv clears AWS settings from the environment so tests don't pick up the developer's config.
func isolateAwsEnv(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0600))
	for _, k := range []string{
		"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestTerragruntIsAddedInUserAgent(t *testing.T) {
	isolateAwsEnv(t)

	var userAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg, err := loadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("id", "secret", "")),
		config.WithRetryMaxAttempts(1),
	)
	require.NoError(t, err)

	_, _ = sts.NewFromConfig(cfg, func(o *sts.Options) { o.BaseEndpoint = aws.String(srv.URL) }).
		GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})

	assert.Contains(t, userAgent, "terragrunt")
}

func TestLoadDefaultConfigFallsBackToDefaultRegion(t *testing.T) {
	isolateAwsEnv(t)

	cfg, err := loadDefaultConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, defaultRegion, cfg.Region)

	cfg, err = loadDefaultConfig(context.Background(), config.WithRegion("eu-west-1"))
	require.NoError(t, err)
	assert.Equal(t, "eu-west-1", cfg.Region)
}

func TestCreateAwsConfigFromConfigUsesCredsFileProfile(t *testing.T) {
	isolateAwsEnv(t)

	credsFile := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(credsFile, []byte("[custom]\naws_access_key_id = AKIDCUSTOM\naws_secret_access_key = secret\n"), 0600))

	cfg, err := CreateAwsConfigFromConfig(&AwsSessionConfig{
		Region:        "eu-west-1",
		Profile:       "custom",
		CredsFilename: credsFile,
	}, options.NewTerragruntOptions())
	require.NoError(t, err)
	assert.Equal(t, "eu-west-1", cfg.Region)

	creds, err := cfg.Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "AKIDCUSTOM", creds.AccessKeyID)
}

// fakeSTS starts an STS endpoint that answers AssumeRole and records each request's form values.
func fakeSTS(t *testing.T) *[]url.Values {
	t.Helper()
	var requests []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		requests = append(requests, r.PostForm)
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials>` +
			`<AccessKeyId>AKIDASSUMED</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>token</SessionToken>` +
			`<Expiration>2099-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL_STS", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDBASE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	return &requests
}

func TestCreateAwsConfigFromConfigAssumesRole(t *testing.T) {
	isolateAwsEnv(t)
	requests := fakeSTS(t)

	cfg, err := CreateAwsConfigFromConfig(&AwsSessionConfig{
		Region:      "eu-west-1",
		RoleArn:     "arn:aws:iam::123456789012:role/test",
		ExternalID:  "ext-id",
		SessionName: "tg-session",
	}, options.NewTerragruntOptions())
	require.NoError(t, err)

	creds, err := cfg.Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "AKIDASSUMED", creds.AccessKeyID)
	require.Len(t, *requests, 1)
	form := (*requests)[0]
	assert.Equal(t, "AssumeRole", form.Get("Action"))
	assert.Equal(t, "arn:aws:iam::123456789012:role/test", form.Get("RoleArn"))
	assert.Equal(t, "ext-id", form.Get("ExternalId"))
	assert.Equal(t, "tg-session", form.Get("RoleSessionName"))
	assert.Equal(t, "3600", form.Get("DurationSeconds"))
}

func TestAssumeRoleDefaultSessionName(t *testing.T) {
	isolateAwsEnv(t)
	requests := fakeSTS(t)

	_, err := CreateAwsConfig(&AwsSessionConfig{Region: "eu-west-1", RoleArn: "arn:aws:iam::123456789012:role/test"}, options.NewTerragruntOptions())
	require.NoError(t, err)

	require.Len(t, *requests, 1)
	assert.Regexp(t, `^[0-9]+$`, (*requests)[0].Get("RoleSessionName"))
}

func TestAwsConfigRoleSources(t *testing.T) {
	const iamRole = "arn:aws:iam::123456789012:role/iam-role"
	const backendRole = "arn:aws:iam::123456789012:role/backend-role"

	testCases := []struct {
		name         string
		iamRole      string
		backendRole  string
		expectedRole string
	}{
		{"no-role", "", "", ""},
		{"iam-role-only", iamRole, "", iamRole},
		{"backend-role-only", "", backendRole, backendRole},
		{"backend-role-wins", iamRole, backendRole, backendRole},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateAwsEnv(t)
			requests := fakeSTS(t)

			terragruntOptions := options.NewTerragruntOptions()
			terragruntOptions.IAMRoleOptions.RoleARN = testCase.iamRole

			cfg, err := CreateAwsConfig(&AwsSessionConfig{Region: "eu-west-1", RoleArn: testCase.backendRole}, terragruntOptions)
			require.NoError(t, err)

			if testCase.expectedRole == "" {
				assert.Empty(t, *requests)
				creds, err := cfg.Credentials.Retrieve(context.Background())
				require.NoError(t, err)
				assert.Equal(t, "AKIDBASE", creds.AccessKeyID)
				return
			}
			require.Len(t, *requests, 1)
			assert.Equal(t, testCase.expectedRole, (*requests)[0].Get("RoleArn"))
		})
	}
}

func TestConfiguredRegionTakesPrecedenceOverEnv(t *testing.T) {
	isolateAwsEnv(t)
	t.Setenv("AWS_REGION", "us-west-2")

	cfg, err := CreateAwsConfigFromConfig(&AwsSessionConfig{Region: "eu-west-1"}, options.NewTerragruntOptions())
	require.NoError(t, err)
	assert.Equal(t, "eu-west-1", cfg.Region)

	cfg, err = CreateAwsConfigFromConfig(&AwsSessionConfig{}, options.NewTerragruntOptions())
	require.NoError(t, err)
	assert.Equal(t, "us-west-2", cfg.Region)
}

func TestNewS3ClientAppliesSessionConfig(t *testing.T) {
	t.Parallel()

	opts := NewS3Client(aws.Config{Region: "eu-west-1"}, &AwsSessionConfig{
		CustomS3Endpoint: "http://localhost:9000",
		S3ForcePathStyle: true,
	}).Options()

	assert.Equal(t, "http://localhost:9000", aws.ToString(opts.BaseEndpoint))
	assert.True(t, opts.UsePathStyle)
}

func TestNewDynamoDBClientAppliesSessionConfig(t *testing.T) {
	t.Parallel()

	opts := NewDynamoDBClient(aws.Config{Region: "eu-west-1"}, &AwsSessionConfig{
		CustomDynamoDBEndpoint:  "http://localhost:8000",
		DisableComputeChecksums: true,
	}).Options()

	assert.Equal(t, "http://localhost:8000", aws.ToString(opts.BaseEndpoint))
	assert.True(t, opts.DisableValidateResponseChecksum)
}

func TestErrorCode(t *testing.T) {
	t.Parallel()

	code, ok := ErrorCode(errors.WithStackTrace(&smithy.GenericAPIError{Code: "NoSuchBucketPolicy"}))
	assert.True(t, ok)
	assert.Equal(t, "NoSuchBucketPolicy", code)

	_, ok = ErrorCode(errors.WithStackTrace(context.DeadlineExceeded))
	assert.False(t, ok)
}

func TestAwsSessionValidationFail(t *testing.T) {
	isolateAwsEnv(t)

	err := ValidateAwsSession(&AwsSessionConfig{
		Region:        "not-existing-region",
		CredsFilename: "/tmp/not-existing-file",
	}, options.NewTerragruntOptions())
	assert.Error(t, err)
}

func TestCreateAwsConfigAssumesIamRoleWithoutSessionConfig(t *testing.T) {
	isolateAwsEnv(t)
	requests := fakeSTS(t)

	terragruntOptions := options.NewTerragruntOptions()
	terragruntOptions.IAMRoleOptions.RoleARN = "arn:aws:iam::123456789012:role/iam-role"

	_, err := CreateAwsConfig(nil, terragruntOptions)
	require.NoError(t, err)

	require.Len(t, *requests, 1)
	assert.Equal(t, "arn:aws:iam::123456789012:role/iam-role", (*requests)[0].Get("RoleArn"))
}

// redirectTransport sends every request to target while leaving the SDK's endpoint resolution untouched.
type redirectTransport struct{ target *url.URL }

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

func TestProfileRoleChainWithoutRegion(t *testing.T) {
	isolateAwsEnv(t)
	requests := fakeSTS(t)
	target, err := url.Parse(os.Getenv("AWS_ENDPOINT_URL_STS"))
	require.NoError(t, err)
	// Resolve the real STS endpoint so a missing region fails as it would against AWS.
	t.Setenv("AWS_ENDPOINT_URL_STS", "")

	// The SDK builds the STS client for a profile's role_arn while loading the config, so the default region must
	// already be set at that point.
	configFile := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(configFile, []byte(
		"[profile base]\naws_access_key_id = AKIDBASE\naws_secret_access_key = secret\n\n"+
			"[profile chained]\nrole_arn = arn:aws:iam::123456789012:role/chained\nsource_profile = base\n"), 0600))
	t.Setenv("AWS_CONFIG_FILE", configFile)

	cfg, err := loadDefaultConfig(context.Background(),
		config.WithSharedConfigProfile("chained"),
		config.WithHTTPClient(&http.Client{Transport: redirectTransport{target: target}}),
	)
	require.NoError(t, err)
	assert.Equal(t, defaultRegion, cfg.Region)

	creds, err := cfg.Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "AKIDASSUMED", creds.AccessKeyID)
	require.Len(t, *requests, 1)
	assert.Equal(t, "arn:aws:iam::123456789012:role/chained", (*requests)[0].Get("RoleArn"))
}
