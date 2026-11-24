package aws_helper

import (
	"context"
	goerrors "errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
	"github.com/gruntwork-io/go-commons/errors"
	"github.com/gruntwork-io/go-commons/version"
	"github.com/gruntwork-io/terragrunt/options"
)

// A representation of the configuration options for an AWS Session
type AwsSessionConfig struct {
	Region                  string
	CustomS3Endpoint        string
	CustomDynamoDBEndpoint  string
	Profile                 string
	RoleArn                 string
	CredsFilename           string
	S3ForcePathStyle        bool
	DisableComputeChecksums bool
	ExternalID              string
	SessionName             string
}

// defaultRegion is used when no region is configured. STS needs a region to resolve its endpoint, including
// the STS clients the SDK builds for profile and web identity credentials while loading the config.
const defaultRegion = "us-east-1"

func loadDefaultConfig(ctx context.Context, opts ...func(*config.LoadOptions) error) (aws.Config, error) {
	opts = append([]func(*config.LoadOptions) error{
		config.WithAppID("terragrunt/" + version.GetVersion()),
		config.WithDefaultRegion(defaultRegion),
	}, opts...)
	return config.LoadDefaultConfig(ctx, opts...)
}

// ErrorCode returns the AWS API error code from err, if it is or wraps an AWS API error.
func ErrorCode(err error) (string, bool) {
	var apiErr smithy.APIError
	if goerrors.As(err, &apiErr) {
		return apiErr.ErrorCode(), true
	}
	return "", false
}

// Returns an AWS config for the given config region (required), profile name (optional), and IAM role to assume
// (optional), ensuring that the credentials are available.
func CreateAwsConfigFromConfig(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(awsSessionConfig.Region)}
	if awsSessionConfig.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(awsSessionConfig.Profile))
	}
	if len(awsSessionConfig.CredsFilename) > 0 {
		opts = append(opts,
			config.WithSharedConfigFiles([]string{awsSessionConfig.CredsFilename}),
			config.WithSharedCredentialsFiles([]string{awsSessionConfig.CredsFilename}),
		)
	}

	cfg, err := loadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return aws.Config{}, errors.WithStackTraceAndPrefix(err, "Error initializing session")
	}

	// Merge the config based IAMRole options into the original one, as the config has higher precedence than CLI.
	iamRoleOptions := terragruntOptions.IAMRoleOptions
	if awsSessionConfig.RoleArn != "" {
		iamRoleOptions = options.MergeIAMRoleOptions(
			iamRoleOptions,
			options.IAMRoleOptions{
				RoleARN:               awsSessionConfig.RoleArn,
				AssumeRoleSessionName: awsSessionConfig.SessionName,
			},
		)
	}

	if iamRoleOptions.RoleARN != "" {
		cfg.Credentials = getSTSCredentialsFromIAMRoleOptions(cfg, iamRoleOptions, awsSessionConfig.ExternalID)
	}
	return cfg, nil
}

func getSTSCredentialsFromIAMRoleOptions(cfg aws.Config, iamRoleOptions options.IAMRoleOptions, externalID string) aws.CredentialsProvider {
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), iamRoleOptions.RoleARN, func(o *stscreds.AssumeRoleOptions) {
		if iamRoleOptions.AssumeRoleDuration > 0 {
			o.Duration = time.Second * time.Duration(iamRoleOptions.AssumeRoleDuration)
		} else {
			o.Duration = time.Second * time.Duration(options.DefaultIAMAssumeRoleDuration)
		}
		o.RoleSessionName = iamRoleOptions.AssumeRoleSessionName
		if o.RoleSessionName == "" {
			o.RoleSessionName = strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
		}
		if externalID != "" {
			o.ExternalID = aws.String(externalID)
		}
	})
	return aws.NewCredentialsCache(provider)
}

// Returns an AWS config. The config is built from either:
//   - The provided AwsSessionConfig struct, which specifies region (required), profile name (optional), and IAM role to
//     assume (optional).
//   - The provided TerragruntOptions struct, which specifies any IAM role to assume (optional).
//
// Note that if the AwsSessionConfig object is null, this will return default credentials using the default
// credentials chain of the AWS SDK.
func CreateAwsConfig(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) (aws.Config, error) {
	var cfg aws.Config
	var err error
	if awsSessionConfig == nil {
		cfg, err = loadDefaultConfig(context.Background())
		if err != nil {
			return aws.Config{}, errors.WithStackTrace(err)
		}
		if terragruntOptions.IAMRoleOptions.RoleARN != "" {
			terragruntOptions.Logger.Debugf("Assuming role %s", terragruntOptions.IAMRoleOptions.RoleARN)
			cfg.Credentials = getSTSCredentialsFromIAMRoleOptions(cfg, terragruntOptions.IAMRoleOptions, "")
		}
	} else {
		cfg, err = CreateAwsConfigFromConfig(awsSessionConfig, terragruntOptions)
		if err != nil {
			return aws.Config{}, errors.WithStackTrace(err)
		}
	}

	if _, err = cfg.Credentials.Retrieve(context.Background()); err != nil {
		msg := "Error finding AWS credentials (did you set the AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables?)"
		if awsSessionConfig != nil && len(awsSessionConfig.CredsFilename) > 0 {
			msg = fmt.Sprintf("Error finding AWS credentials in file '%s' (did you set the correct file name and/or profile?)", awsSessionConfig.CredsFilename)
		}

		return aws.Config{}, errors.WithStackTraceAndPrefix(err, "%s", msg)
	}

	return cfg, nil
}

// NewS3Client returns an S3 client for the given config, applying any custom endpoint and path style settings.
func NewS3Client(cfg aws.Config, awsSessionConfig *AwsSessionConfig) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if awsSessionConfig == nil {
			return
		}
		if awsSessionConfig.CustomS3Endpoint != "" {
			o.BaseEndpoint = aws.String(awsSessionConfig.CustomS3Endpoint)
		}
		o.UsePathStyle = awsSessionConfig.S3ForcePathStyle
	})
}

// NewDynamoDBClient applies custom endpoint settings and skips response checksum validation when DisableComputeChecksums is set.
func NewDynamoDBClient(cfg aws.Config, awsSessionConfig *AwsSessionConfig) *dynamodb.Client {
	return dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		if awsSessionConfig == nil {
			return
		}
		if awsSessionConfig.CustomDynamoDBEndpoint != "" {
			o.BaseEndpoint = aws.String(awsSessionConfig.CustomDynamoDBEndpoint)
		}
		o.DisableValidateResponseChecksum = awsSessionConfig.DisableComputeChecksums
	})
}

// Make API calls to AWS to assume the IAM role specified and return the temporary AWS credentials to use that role
func AssumeIamRole(iamRoleOpts options.IAMRoleOptions) (*types.Credentials, error) {
	cfg, err := loadDefaultConfig(context.Background())
	if err != nil {
		return nil, errors.WithStackTrace(err)
	}

	if _, err = cfg.Credentials.Retrieve(context.Background()); err != nil {
		return nil, errors.WithStackTraceAndPrefix(err, "Error finding AWS credentials (did you set the AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables?)")
	}

	sessionName := options.GetDefaultIAMAssumeRoleSessionName()
	if iamRoleOpts.AssumeRoleSessionName != "" {
		sessionName = iamRoleOpts.AssumeRoleSessionName
	}

	sessionDurationSeconds := int64(options.DefaultIAMAssumeRoleDuration)
	if iamRoleOpts.AssumeRoleDuration != 0 {
		sessionDurationSeconds = iamRoleOpts.AssumeRoleDuration
	}

	output, err := sts.NewFromConfig(cfg).AssumeRole(context.Background(), &sts.AssumeRoleInput{
		RoleArn:         aws.String(iamRoleOpts.RoleARN),
		RoleSessionName: aws.String(sessionName),
		DurationSeconds: aws.Int32(int32(sessionDurationSeconds)),
	})
	if err != nil {
		return nil, errors.WithStackTrace(err)
	}

	return output.Credentials, nil
}

// Return the AWS caller identity associated with the current set of credentials
func GetAWSCallerIdentity(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) (sts.GetCallerIdentityOutput, error) {
	cfg, err := CreateAwsConfig(awsSessionConfig, terragruntOptions)
	if err != nil {
		return sts.GetCallerIdentityOutput{}, errors.WithStackTrace(err)
	}

	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	if err != nil {
		return sts.GetCallerIdentityOutput{}, errors.WithStackTrace(err)
	}

	return *identity, nil
}

// ValidateAwsSession - Validate if current AWS session is valid
func ValidateAwsSession(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) error {
	// read the caller identity to check if the credentials are valid
	_, err := GetAWSCallerIdentity(awsSessionConfig, terragruntOptions)
	return err
}

// Get the AWS Partition of the current session configuration
func GetAWSPartition(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) (string, error) {
	identity, err := GetAWSCallerIdentity(awsSessionConfig, terragruntOptions)
	if err != nil {
		return "", errors.WithStackTrace(err)
	}

	parsed, err := arn.Parse(aws.ToString(identity.Arn))
	if err != nil {
		return "", errors.WithStackTrace(err)
	}
	return parsed.Partition, nil
}

// Get the AWS account ID of the current session configuration
func GetAWSAccountID(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) (string, error) {
	identity, err := GetAWSCallerIdentity(awsSessionConfig, terragruntOptions)
	if err != nil {
		return "", errors.WithStackTrace(err)
	}

	return aws.ToString(identity.Account), nil
}

// Get the ARN of the AWS identity associated with the current set of credentials
func GetAWSIdentityArn(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) (string, error) {
	identity, err := GetAWSCallerIdentity(awsSessionConfig, terragruntOptions)
	if err != nil {
		return "", errors.WithStackTrace(err)
	}

	return aws.ToString(identity.Arn), nil
}

// Get the AWS user ID of the current session configuration
func GetAWSUserID(awsSessionConfig *AwsSessionConfig, terragruntOptions *options.TerragruntOptions) (string, error) {
	identity, err := GetAWSCallerIdentity(awsSessionConfig, terragruntOptions)
	if err != nil {
		return "", errors.WithStackTrace(err)
	}

	return aws.ToString(identity.UserId), nil
}

// Assume an IAM role, if one is specified, by making API calls to Amazon STS and setting the environment variables
// we get back inside of terragruntOptions.Env
func AssumeRoleAndUpdateEnvIfNecessary(terragruntOptions *options.TerragruntOptions) error {
	iamRoleOpts := terragruntOptions.IAMRoleOptions
	if iamRoleOpts.RoleARN == "" {
		return nil
	}

	terragruntOptions.Logger.Debugf("Assuming IAM role %s with a session duration of %d seconds.", iamRoleOpts.RoleARN, iamRoleOpts.AssumeRoleDuration)
	creds, err := AssumeIamRole(iamRoleOpts)
	if err != nil {
		return err
	}

	terragruntOptions.Env["AWS_ACCESS_KEY_ID"] = aws.ToString(creds.AccessKeyId)
	terragruntOptions.Env["AWS_SECRET_ACCESS_KEY"] = aws.ToString(creds.SecretAccessKey)
	terragruntOptions.Env["AWS_SESSION_TOKEN"] = aws.ToString(creds.SessionToken)
	terragruntOptions.Env["AWS_SECURITY_TOKEN"] = aws.ToString(creds.SessionToken)

	return nil
}
