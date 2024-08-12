package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// settings will handle the user input arguments when running the program
type settings struct {
	showDebugLog   bool
	awsProfileName string
	outputFileName string
}

// application will hold all the dependencies that wil be used in many functions
type application struct {
	logger       *zap.Logger
	lambdaClient *lambda.Client
	cwlogsClient *cloudwatchlogs.Client
}

// lambdaFunctionDetails holds the details of the lambda function that will be printed
// the title tag is the title of the column of the resulting CSV file
type lambdaFunctionDetails struct {
	name         string `title:"Function Name"`
	arn          string `title:"Function ARN"`
	description  string `title:"Function Description"`
	lastModified string `title:"Last Modified"`
	iamRole      string `title:"IAM Role"`
	runtime      string `title:"Runtime"`
	lastInvoked  string `title:"Last Invoked"`
}

// getTitleFields will return a list of strings that is populated by the struct title tag
func (l lambdaFunctionDetails) getTitleFields() []string {
	var titles []string

	value := reflect.ValueOf(l)
	for i := 0; i < value.NumField(); i++ {
		title := value.Type().Field(i).Tag.Get("title")
		titles = append(titles, title)
	}

	return titles
}

const lambdaLogGroupPrefix = "/aws/lambda/"

func main() {
	var stg settings
	flag.BoolVar(&stg.showDebugLog, "show-debug-log", false, "Show debug log")
	flag.StringVar(&stg.awsProfileName, "aws-profile", "default", "AWS Profile Name")
	flag.StringVar(&stg.outputFileName, "out-name", "lambda-list.csv", "The name of the output file")
	flag.Parse()

	logger := createLogger(stg.showDebugLog)
	defer logger.Sync()

	logger.Debug("loading default config")
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithSharedConfigProfile(stg.awsProfileName))
	if err != nil {
		logger.Fatal("error when loading default config",
			zap.Error(err),
		)
	}

	lambdaClient := lambda.NewFromConfig(cfg)
	cwlogsClient := cloudwatchlogs.NewFromConfig(cfg)

	app := &application{
		logger:       logger,
		lambdaClient: lambdaClient,
		cwlogsClient: cwlogsClient,
	}

	logger.Info("getting function details for all lambda functions")
	lambdaFunctionsDetailsList, err := app.getAllLambdaFunctionsDetailsList()
	if err != nil {
		logger.Fatal("error when listing lambda function details",
			zap.Error(err),
		)
	}
	logger.Debug("got all lambda function details",
		zap.Int("length", len(lambdaFunctionsDetailsList)),
	)

	logger.Info("getting last invoke time for all lambda functions")
	wg := &sync.WaitGroup{}
	app.getAllLambdaFunctionsLastInvokeTimeBackground(lambdaFunctionsDetailsList, wg)
	wg.Wait()

	logger.Sugar().Infof("writing the output to %s", stg.outputFileName)
	f, err := os.Create(stg.outputFileName)
	if err != nil {
		logger.Error("error when creating a file",
			zap.Error(err),
		)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	titles := lambdaFunctionsDetailsList[0].getTitleFields()
	err = w.Write(titles)
	if err != nil {
		logger.Error("error when writing title")
	}

	for _, lambdaDetails := range lambdaFunctionsDetailsList {
		record := []string{lambdaDetails.name, lambdaDetails.arn, lambdaDetails.description, lambdaDetails.lastModified, lambdaDetails.iamRole, lambdaDetails.runtime, lambdaDetails.lastInvoked}
		err := w.Write(record)
		if err != nil {
			logger.Error("error when writing the entry",
				zap.String("function_name", lambdaDetails.name),
				zap.Error(err),
			)
		}
	}

	logger.Info("all the function details have been written to the output",
		zap.String("file name", stg.outputFileName),
		zap.Int("number of functions", len(lambdaFunctionsDetailsList)),
	)
}

func (app *application) getAllLambdaFunctionsDetailsList() ([]lambdaFunctionDetails, error) {
	input := &lambda.ListFunctionsInput{}
	var lambdaFunctionsDetailsList []lambdaFunctionDetails

	for {
		out, err := app.lambdaClient.ListFunctions(context.Background(), input)
		if err != nil {
			return nil, err
		}

		for _, functionDetail := range out.Functions {
			l := lambdaFunctionDetails{
				name:         *functionDetail.FunctionName,
				arn:          *functionDetail.FunctionArn,
				description:  *functionDetail.Description,
				lastModified: *functionDetail.LastModified,
				iamRole:      *functionDetail.Role,
				runtime:      string(functionDetail.Runtime),
			}

			lambdaFunctionsDetailsList = append(lambdaFunctionsDetailsList, l)
		}

		if out.NextMarker != nil {
			input.Marker = out.NextMarker
			continue
		} else {
			break
		}
	}

	return lambdaFunctionsDetailsList, nil
}

func (app *application) getLambdaFunctionLastInvokeTimeBackground(functionName string, index int, outputList []lambdaFunctionDetails, wg *sync.WaitGroup) {
	defer wg.Done()
	logGroupName := fmt.Sprintf("%s%s", lambdaLogGroupPrefix, functionName)

	input := &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName: aws.String(logGroupName),
		Descending:   aws.Bool(false),
		Limit:        aws.Int32(1),
		OrderBy:      types.OrderByLastEventTime,
	}

	out, err := app.cwlogsClient.DescribeLogStreams(context.Background(), input)
	if err != nil {
		app.logger.Debug("error when describing log stream",
			zap.Error(err),
			zap.String("log group name", logGroupName),
		)
	}

	if out != nil && out.LogStreams != nil && out.LogStreams[0].LastEventTimestamp != nil {
		lastEventTimestampInSeconds := *out.LogStreams[0].LastEventTimestamp / 1000
		t := time.Unix(lastEventTimestampInSeconds, 0)

		outputList[index].lastInvoked = t.Format("2006-01-02T15:04:05-07:00")
		app.logger.Debug("last invoke time info",
			zap.Int64("*out.LogStreams[0].LastEventTimestamp", *out.LogStreams[0].LastEventTimestamp/1000),
			zap.Int64("lastEventTimestampInSeconds", lastEventTimestampInSeconds),
			zap.String("formatted time", t.Format("2006-01-02T15:04:05-07:00")),
			zap.String("outputList[index].lastInvoked", outputList[index].lastInvoked),
		)
	} else {
		app.logger.Debug("cannot find the last invoke time for lambda",
			zap.String("function_name", functionName),
		)

		outputList[index].lastInvoked = "Not Found"
	}
}

func (app *application) getAllLambdaFunctionsLastInvokeTimeBackground(outputlist []lambdaFunctionDetails, wg *sync.WaitGroup) {
	for i, lambdaDetails := range outputlist {
		wg.Add(1)
		go app.getLambdaFunctionLastInvokeTimeBackground(lambdaDetails.name, i, outputlist, wg)
	}
}

func createLogger(showDebug bool) *zap.Logger {
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	level := zap.NewAtomicLevelAt(zap.InfoLevel)
	if showDebug {
		level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}

	config := zap.Config{
		Level:             level,
		Development:       false,
		DisableCaller:     false,
		DisableStacktrace: false,
		Encoding:          "console",
		EncoderConfig:     encoderConfig,
		OutputPaths: []string{
			"stdout",
		},
		ErrorOutputPaths: []string{
			"stderr",
		},
	}

	return zap.Must(config.Build())
}
