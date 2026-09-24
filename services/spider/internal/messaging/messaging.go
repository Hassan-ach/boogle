package messaging

type MessagingQueue interface {
	NewMessagingChannel() (MessagingChannel, error)
	DeclareQueue(queueName string) error
	Close()
}

type MessagingChannel interface {
	Publish(queueName string, body []byte) error
	Consume(queueName string, handler func([]byte) error) error
	Close()
}
