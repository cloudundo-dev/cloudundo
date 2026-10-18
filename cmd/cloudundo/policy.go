package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/cloudundo/cloudundo/internal/policy"
)

// cmdPolicy prints the IAM policy for the agent's credentials. The JSON
// goes to stdout so it can be saved to a file; how to use it goes to
// stderr.
func cmdPolicy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o policy.Options
	fs.StringVar(&o.Account, "account", "", "limit the policy to this AWS account id")
	fs.StringVar(&o.Region, "region", "", "limit the policy to this region")
	fs.BoolVar(&o.KeepVersions, "keep-versions", false, "also forbid removing any S3 object version in the account")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: cloudundo policy [--account ID] [--region R] [--keep-versions] > cloudundo-policy.json")
		return 2
	}
	if err := o.Validate(); err != nil {
		fmt.Fprintln(stderr, "cloudundo policy:", err)
		return 2
	}
	fmt.Fprint(stdout, policy.Build(o).JSON())

	fmt.Fprintf(stderr, `
How to use it (run these yourself, with your own admin credentials, not the agent's):

  cloudundo policy%s > cloudundo-policy.json
  aws iam put-user-policy --user-name <the agent's IAM user> \
      --policy-name cloudundo-guard --policy-document file://cloudundo-policy.json
`, flagsText(o))
	if o.Account != "" && o.Region != "" {
		b := o.BackupBucket()
		fmt.Fprintf(stderr, `
Create the backup bucket first, versioned, because the policy stops the agent's
credentials from changing its versioning later:

  aws s3api create-bucket --bucket %[1]s --region %[2]s%[3]s
  aws s3api put-public-access-block --bucket %[1]s --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  aws s3api put-bucket-versioning --bucket %[1]s --versioning-configuration Status=Enabled
`, b, o.Region, locationFlag(o.Region))
	}
	fmt.Fprintln(stderr, `
To clean up old backups later, use your own credentials; the agent's cannot.`)
	return 0
}

func flagsText(o policy.Options) string {
	s := ""
	if o.Account != "" {
		s += " --account " + o.Account
	}
	if o.Region != "" {
		s += " --region " + o.Region
	}
	if o.KeepVersions {
		s += " --keep-versions"
	}
	return s
}

func locationFlag(region string) string {
	if region == "us-east-1" {
		return ""
	}
	return " --create-bucket-configuration LocationConstraint=" + region
}
