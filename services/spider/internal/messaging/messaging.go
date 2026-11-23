package messaging

import "context"

type MessagingQueue interface {
	NewMessagingChannel() (MessagingChannel, error)
	DeclareQueue(queueName string) error
	Close()
}

type MessagingChannel interface {
	Publish(ctx context.Context, queueName string, body []byte) error
	Consume(queueName string, handler func([]byte) error) error
	Close()
}
