# AWS Lambda Lister
Tool to list all of your AWS Lambda Functions and their last invocation time and output the result in a CSV file

## Getting started
By default, it will use your "default" profile in your AWS CLI configuration
```shell
go run main.go
```

If you want to use different profile, you can use `--aws-profile` argument
```shell
go run main.go --aws-profile <your-profile-name>
```