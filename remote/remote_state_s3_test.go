package remote

import (
	goerrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
	"github.com/gruntwork-io/go-commons/errors"
	"github.com/gruntwork-io/terragrunt/aws_helper"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigValuesEqual(t *testing.T) {
	t.Parallel()

	terragruntOptions, err := options.NewTerragruntOptionsForTest("remote_state_test")
	require.Nil(t, err, "Unexpected error creating NewTerragruntOptionsForTest: %v", err)

	testCases := []struct {
		name          string
		config        map[string]interface{}
		backend       *TerraformBackend
		shouldBeEqual bool
	}{
		{
			"equal-both-empty",
			map[string]interface{}{},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{}},
			true,
		},
		{
			"equal-empty-and-nil",
			map[string]interface{}{},
			nil,
			true,
		},
		{
			"equal-empty-and-nil-backend-config",
			map[string]interface{}{},
			&TerraformBackend{Type: "s3"},
			true,
		},
		{
			"equal-one-key",
			map[string]interface{}{"foo": "bar"},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"foo": "bar"}},
			true,
		},
		{
			"equal-multiple-keys",
			map[string]interface{}{"foo": "bar", "baz": []string{"a", "b", "c"}, "blah": 123, "bool": true},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"foo": "bar", "baz": []string{"a", "b", "c"}, "blah": 123, "bool": true}},
			true,
		},
		{
			"equal-encrypt-bool-handling",
			map[string]interface{}{"encrypt": true},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"encrypt": "true"}},
			true,
		},
		{
			"equal-general-bool-handling",
			map[string]interface{}{"something": true, "encrypt": true},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"something": "true", "encrypt": "true"}},
			true,
		},
		{
			"equal-ignore-s3-tags",
			map[string]interface{}{"foo": "bar", "s3_bucket_tags": []map[string]string{{"foo": "bar"}}},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"foo": "bar"}},
			true,
		},
		{
			"equal-ignore-dynamodb-tags",
			map[string]interface{}{"dynamodb_table_tags": []map[string]string{{"foo": "bar"}}},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{}},
			true,
		},
		{
			"equal-ignore-accesslogging-bucket-tags",
			map[string]interface{}{"accesslogging_bucket_tags": []map[string]string{{"foo": "bar"}}},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{}},
			true,
		},
		{
			"unequal-wrong-backend",
			map[string]interface{}{"foo": "bar"},
			&TerraformBackend{Type: "wrong", Config: map[string]interface{}{"foo": "bar"}},
			false,
		},
		{
			"unequal-values",
			map[string]interface{}{"foo": "bar"},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"foo": "different"}},
			false,
		},
		{
			"unequal-non-empty-config-nil",
			map[string]interface{}{"foo": "bar"},
			nil,
			false,
		},
		{
			"unequal-general-bool-handling",
			map[string]interface{}{"something": true},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"something": "false"}},
			false,
		},
		{
			"equal-null-ignored",
			map[string]interface{}{"something": "foo"},
			&TerraformBackend{Type: "s3", Config: map[string]interface{}{"something": "foo", "ignored-because-null": nil}},
			true,
		},
	}

	for _, testCase := range testCases {
		// Save the testCase in local scope so all the t.Run calls don't end up with the last item in the list
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			actual := configValuesEqual(testCase.config, testCase.backend, terragruntOptions)
			assert.Equal(t, testCase.shouldBeEqual, actual)
		})
	}
}

func TestForcePathStyleClientSession(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		config   map[string]interface{}
		expected bool
	}{
		{
			"path-style-true",
			map[string]interface{}{"force_path_style": true},
			true,
		},
		{
			"path-style-false",
			map[string]interface{}{"force_path_style": false},
			false,
		},
		{
			"path-style-non-existent",
			map[string]interface{}{},
			false,
		},
	}

	for _, testCase := range testCases {
		// The following is necessary to make sure testCase's values don't
		// get updated due to concurrency within the scope of t.Run(..) below
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			s3ConfigExtended, err := ParseExtendedS3Config(testCase.config)
			require.Nil(t, err, "Unexpected error parsing config for test: %v", err)

			sessionConfig := s3ConfigExtended.GetAwsSessionConfig()
			assert.Equal(t, testCase.expected, sessionConfig.S3ForcePathStyle)
			assert.Equal(t, testCase.expected, aws_helper.NewS3Client(aws.Config{Region: "us-east-1"}, sessionConfig).Options().UsePathStyle)
		})
	}
}

func TestGetAwsSessionConfig(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		config map[string]interface{}
	}{
		{
			"all-values",
			map[string]interface{}{"region": "foo", "endpoint": "bar", "profile": "baz", "role_arn": "arn::it", "shared_credentials_file": "my-file", "force_path_style": true},
		},
		{
			"no-values",
			map[string]interface{}{},
		},
		{
			"extra-values",
			map[string]interface{}{"something": "unexpected", "region": "foo", "endpoint": "bar", "dynamodb_endpoint": "foobar", "profile": "baz", "role_arn": "arn::it", "shared_credentials_file": "my-file", "force_path_style": false},
		},
	}

	for _, testCase := range testCases {
		// The following is necessary to make sure testCase's values don't
		// get updated due to concurrency within the scope of t.Run(..) below
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			s3ConfigExtended, err := ParseExtendedS3Config(testCase.config)
			require.Nil(t, err, "Unexpected error parsing config for test: %v", err)

			expected := &aws_helper.AwsSessionConfig{
				Region:                  s3ConfigExtended.remoteStateConfigS3.Region,
				CustomS3Endpoint:        s3ConfigExtended.remoteStateConfigS3.Endpoint,
				CustomDynamoDBEndpoint:  s3ConfigExtended.remoteStateConfigS3.DynamoDBEndpoint,
				Profile:                 s3ConfigExtended.remoteStateConfigS3.Profile,
				RoleArn:                 s3ConfigExtended.remoteStateConfigS3.RoleArn,
				CredsFilename:           s3ConfigExtended.remoteStateConfigS3.CredsFilename,
				S3ForcePathStyle:        s3ConfigExtended.remoteStateConfigS3.S3ForcePathStyle,
				DisableComputeChecksums: s3ConfigExtended.DisableAWSClientChecksums,
			}

			actual := s3ConfigExtended.GetAwsSessionConfig()
			assert.Equal(t, expected, actual)
		})
	}
}

func TestGetTerraformInitArgs(t *testing.T) {
	t.Parallel()

	initializer := S3Initializer{}

	testCases := []struct {
		name          string
		config        map[string]interface{}
		expected      map[string]interface{}
		shouldBeEqual bool
	}{
		{
			"empty-no-values",
			map[string]interface{}{},
			map[string]interface{}{},
			true,
		},
		{
			"valid-s3-configuration-keys",
			map[string]interface{}{
				"bucket":  "foo",
				"encrypt": "bar",
				"key":     "baz",
				"region":  "quux",
			},
			map[string]interface{}{
				"bucket":  "foo",
				"encrypt": "bar",
				"key":     "baz",
				"region":  "quux",
			},
			true,
		},
		{
			"terragrunt-keys-filtered",
			map[string]interface{}{
				"bucket":         "foo",
				"encrypt":        "bar",
				"key":            "baz",
				"region":         "quux",
				"s3_bucket_tags": map[string]string{},
			},
			map[string]interface{}{
				"bucket":  "foo",
				"encrypt": "bar",
				"key":     "baz",
				"region":  "quux",
			},
			true,
		},
		{
			"empty-no-values-all-terragrunt-keys-filtered",
			map[string]interface{}{
				"s3_bucket_tags":                     map[string]string{},
				"dynamodb_table_tags":                map[string]string{},
				"accesslogging_bucket_tags":          map[string]string{},
				"skip_bucket_versioning":             true,
				"skip_bucket_ssencryption":           false,
				"skip_bucket_root_access":            false,
				"skip_bucket_enforced_tls":           false,
				"skip_bucket_public_access_blocking": false,
				"disable_bucket_update":              true,
				"enable_lock_table_ssencryption":     true,
				"disable_aws_client_checksums":       false,
				"accesslogging_bucket_name":          "test",
				"accesslogging_target_prefix":        "test",
			},
			map[string]interface{}{},
			true,
		},
		{
			"lock-table-replaced-with-dynamodb-table",
			map[string]interface{}{
				"bucket":     "foo",
				"encrypt":    "bar",
				"key":        "baz",
				"region":     "quux",
				"lock_table": "xyzzy",
			},
			map[string]interface{}{
				"bucket":         "foo",
				"encrypt":        "bar",
				"key":            "baz",
				"region":         "quux",
				"dynamodb_table": "xyzzy",
			},
			true,
		},
		{
			"dynamodb-table-not-replaced-with-lock-table",
			map[string]interface{}{
				"bucket":         "foo",
				"encrypt":        "bar",
				"key":            "baz",
				"region":         "quux",
				"dynamodb_table": "xyzzy",
			},
			map[string]interface{}{
				"bucket":     "foo",
				"encrypt":    "bar",
				"key":        "baz",
				"region":     "quux",
				"lock_table": "xyzzy",
			},
			false,
		},
	}

	for _, testCase := range testCases {
		// Save the testCase in local scope so all the t.Run calls don't end up with the last item in the list
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			actual := initializer.GetTerraformInitArgs(testCase.config)

			if !testCase.shouldBeEqual {
				assert.NotEqual(t, testCase.expected, actual)
				return
			}
			assert.Equal(t, testCase.expected, actual)
		})
	}
}

// Test to validate cases when is not possible to read all S3 configurations
// https://github.com/gruntwork-io/terragrunt/issues/2109
func TestNegativePublicAccessResponse(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		response *s3.GetPublicAccessBlockOutput
	}{
		{
			name: "nil-response",
			response: &s3.GetPublicAccessBlockOutput{
				PublicAccessBlockConfiguration: nil,
			},
		},
		{
			name: "legacy-bucket",
			response: &s3.GetPublicAccessBlockOutput{
				PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
					BlockPublicAcls:       nil,
					BlockPublicPolicy:     nil,
					IgnorePublicAcls:      nil,
					RestrictPublicBuckets: nil,
				},
			},
		},
		{
			name: "only-block-public-acls",
			response: &s3.GetPublicAccessBlockOutput{
				PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
					BlockPublicAcls:       aws.Bool(true),
					BlockPublicPolicy:     aws.Bool(false),
					IgnorePublicAcls:      aws.Bool(false),
					RestrictPublicBuckets: aws.Bool(false),
				},
			},
		},
		{
			name: "false-response",
			response: &s3.GetPublicAccessBlockOutput{
				PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
					BlockPublicAcls:       aws.Bool(false),
					BlockPublicPolicy:     aws.Bool(false),
					IgnorePublicAcls:      aws.Bool(false),
					RestrictPublicBuckets: aws.Bool(false),
				},
			},
		},
	}
	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			response, err := validatePublicAccessBlock(testCase.response)
			assert.NoError(t, err)
			assert.False(t, response)
		})
	}
}

func TestIsBucketCreationErrorRetriable(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{"non-sdk-error", goerrors.New("policy mismatch"), true},
		{"retriable-api-error", &smithy.GenericAPIError{Code: "InternalError"}, true},
		{"other-api-error", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"sdk-error-without-api-error", &smithy.OperationError{ServiceID: "S3", OperationName: "CreateBucket", Err: goerrors.New("dial tcp: no such host")}, true},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, testCase.expected, isBucketCreationErrorRetriable(errors.WithStackTrace(testCase.err)))
		})
	}
}

func TestPositivePublicAccessResponse(t *testing.T) {
	t.Parallel()

	response, err := validatePublicAccessBlock(&s3.GetPublicAccessBlockOutput{
		PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})
	assert.NoError(t, err)
	assert.True(t, response)
}

// newTestS3Client returns an S3 client that sends every request to handler.
func newTestS3Client(t *testing.T, handler http.HandlerFunc) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("id", "secret", ""),
		Retryer:      aws.NopRetryer{},
	})
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("<Error><Code>" + code + "</Code></Error>"))
}

func TestCheckIfSSEForS3EnabledWithoutConfiguration(t *testing.T) {
	t.Parallel()

	// Some S3-compatible stores answer GetBucketEncryption with 200 and an empty body.
	client := newTestS3Client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	config := &ExtendedRemoteStateConfigS3{remoteStateConfigS3: RemoteStateConfigS3{Bucket: "bucket"}}
	terragruntOptions, err := options.NewTerragruntOptionsForTest("s3_client_test")
	require.NoError(t, err)

	enabled, err := checkIfSSEForS3Enabled(client, config, terragruntOptions)
	require.NoError(t, err)
	assert.False(t, enabled)
}

func TestCheckIfBucketPolicyStatementExists(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		handler  http.HandlerFunc
		expected bool
	}{
		{"no-policy", func(w http.ResponseWriter, r *http.Request) {
			writeS3Error(w, http.StatusNotFound, "NoSuchBucketPolicy")
		}, false},
		{"other-statement", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"Version":"2012-10-17","Statement":[{"Sid":"Other","Effect":"Allow","Action":"s3:*","Resource":"*"}]}`))
		}, false},
		{"matching-statement", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"Version":"2012-10-17","Statement":[{"Sid":"RootAccess","Effect":"Allow","Action":"s3:*","Resource":"*"}]}`))
		}, true},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			exists, err := checkIfBucketPolicyStatementExists(newTestS3Client(t, testCase.handler), "bucket", SidRootPolicy)
			require.NoError(t, err)
			assert.Equal(t, testCase.expected, exists)
		})
	}
}

func TestCreateBucketKeepsOriginalErrorWhenAccessCheckFails(t *testing.T) {
	t.Parallel()

	client := newTestS3Client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writeS3Error(w, http.StatusConflict, "BucketAlreadyExists")
			return
		}
		writeS3Error(w, http.StatusForbidden, "AccessDenied")
	})

	config := &ExtendedRemoteStateConfigS3{remoteStateConfigS3: RemoteStateConfigS3{Bucket: "bucket", Key: "state"}}
	terragruntOptions, err := options.NewTerragruntOptionsForTest("s3_client_test")
	require.NoError(t, err)

	err = CreateS3BucketWithVersioningSSEncryptionAndAccessLogging(client, config, terragruntOptions)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BucketAlreadyExists")
	assert.Contains(t, err.Error(), "AccessDenied")
}

func TestCreateS3BucketLocationConstraint(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		region             string
		expectedConstraint string
	}{
		{"us-east-1", ""},
		{"eu-west-1", "<LocationConstraint>eu-west-1</LocationConstraint>"},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.region, func(t *testing.T) {
			t.Parallel()

			var body string
			client := newTestS3Client(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				body = string(b)
			})
			client = s3.New(client.Options(), func(o *s3.Options) { o.Region = testCase.region })

			terragruntOptions, err := options.NewTerragruntOptionsForTest("s3_client_test")
			require.NoError(t, err)
			require.NoError(t, CreateS3Bucket(client, aws.String("bucket"), terragruntOptions))

			if testCase.expectedConstraint == "" {
				assert.NotContains(t, body, "LocationConstraint")
			} else {
				assert.Contains(t, body, testCase.expectedConstraint)
			}
		})
	}
}
