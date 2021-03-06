package main

import (
	"bytes"
	"errors"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sqs"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

type Worker struct{}

func (w Worker) Work(ch chan Worker) {
	defer func() {
		ch <- w
	}()

	log.Println("Requesting message")
	response, err := client.ReceiveMessage(&sqs.ReceiveMessageInput{
		MaxNumberOfMessages: aws.Int64(1),
		QueueUrl:            aws.String(workerConfig.queueUrl),
		// QueueName:           aws.String(workerConfig.queueName),
		WaitTimeSeconds:   aws.Int64(20),
		VisibilityTimeout: aws.Int64(int64(workerConfig.timeout + 5)),
		AttributeNames: []*string{
			aws.String("ApproximateReceiveCount"),
		},
	})
	if err != nil {
		log.Println(err)
		return
	}

	if len(response.Messages) > 0 {
		msg := response.Messages[0]
		log.Printf("%+v\n\n", msg)
		err = w.handleMessage(msg)
		if err != nil {
			// It'll be picked up again after the visibility timeout. If we're on ElasticMQ, which doesn't have dead letter queue support, we need to delete the message if it reaches the maximum receive count.
			log.Println(err)

			if workerConfig.elastic {
				if receiveCount, ok := msg.Attributes["ApproximateReceiveCount"]; ok {
					parsed, err := strconv.ParseInt(*receiveCount, 10, 64)
					if err != nil {
						log.Println(err)
					} else {
						if parsed >= int64(workerConfig.maxReceiveCount) {
							log.Println("Sending message to dead letter queue")
							_, err = client.SendMessage(&sqs.SendMessageInput{
								MessageBody: msg.Body,
								QueueUrl:    aws.String(workerConfig.deadQueueUrl),
							})
							if err != nil {
								log.Println(err)
							} else {
								err = deleteMessage(msg)
								if err != nil {
									log.Println(err)
								}
							}

						}
					}
				}
			}
		} else {
			// Delete from the queue
			err = deleteMessage(msg)

			if err != nil {
				log.Println(err)
			}
		}
	}
}

func deleteMessage(msg *sqs.Message) error {
	_, err := client.DeleteMessage(&sqs.DeleteMessageInput{
		QueueUrl:      aws.String(workerConfig.queueUrl),
		ReceiptHandle: msg.ReceiptHandle,
	})
	return err
}

func (Worker) handleMessage(msg *sqs.Message) error {
	now := time.Now()
	if StatsEnabled {
		go StatsClient.Incr("received", nil)
	}

	body := *msg.Body

	reader := bytes.NewReader([]byte(body))

	req, err := http.NewRequest("POST", workerConfig.workerUrl, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Aws-Sqsd-Receive-Count", "2")

	client := http.Client{
		Timeout: time.Duration(time.Duration(workerConfig.timeout) * time.Second),
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if StatsEnabled {
		defer func() {
			// Get difference in milliseconds
			diff := (time.Now().UnixNano() - now.UnixNano()) / 1000000
			go StatsClient.Histogram("response_time", float64(diff), nil)
		}()
	}
	if res.StatusCode > 299 || res.StatusCode < 200 {
		if StatsEnabled {
			go StatsClient.Incr("error", []string{res.Status})
		}
		io.Copy(os.Stdout, res.Body)
		return errors.New("Host returned error status (" + res.Status + ")")
	} else {
		if StatsEnabled {
			go StatsClient.Incr("success", nil)
		}
		return nil
	}
}
