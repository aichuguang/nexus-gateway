package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

func sendSMTP(ctx context.Context, c Channel, event Event) error {
	return sendSMTPWithDial(ctx, c, event, publicDialContext, nil)
}

// The injected dial and trust roots are only a transport test seam. Production
// always uses publicDialContext and system certificate roots above.
func sendSMTPWithDial(ctx context.Context, c Channel, event Event, dial func(context.Context, string, string) (net.Conn, error), config *tls.Config) error {
	connection, err := dial(ctx, "tcp", net.JoinHostPort(c.SMTPHost, strconv.Itoa(c.SMTPPort)))
	if err != nil {
		return &DeliveryError{Message: "SMTP 连接失败或目标不允许", Temporary: ctx.Err() == nil}
	}
	rawConnection := connection
	defer rawConnection.Close()
	stop := context.AfterFunc(ctx, func() { _ = rawConnection.Close() })
	defer stop()
	deadline := time.Now().Add(8 * time.Second)
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}
	_ = connection.SetDeadline(deadline)
	if config == nil {
		config = &tls.Config{ServerName: c.SMTPHost, MinVersion: tls.VersionTLS12}
	}
	if c.SMTPTLS == "tls" {
		secured := tls.Client(connection, config)
		if secured.HandshakeContext(ctx) != nil {
			return errors.New("SMTP TLS 证书或握手校验失败")
		}
		connection = secured
	}
	client, err := smtp.NewClient(connection, c.SMTPHost)
	if err != nil {
		return errors.New("SMTP 握手失败")
	}
	defer client.Close()
	if c.SMTPTLS == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP 服务不支持必需的 STARTTLS")
		}
		if client.StartTLS(config) != nil {
			return errors.New("SMTP STARTTLS 证书或握手校验失败")
		}
	}
	if c.SMTPUsername != "" && client.Auth(smtp.PlainAuth("", c.SMTPUsername, c.SMTPPassword, c.SMTPHost)) != nil {
		return errors.New("SMTP 身份验证失败")
	}
	if client.Mail(c.SMTPFrom) != nil {
		return errors.New("SMTP 发件地址被拒绝")
	}
	for _, recipient := range c.SMTPTo {
		if client.Rcpt(recipient) != nil {
			return errors.New("SMTP 收件地址被拒绝")
		}
	}
	writer, err := client.Data()
	if err != nil {
		return errors.New("SMTP 无法接收邮件正文")
	}
	subject := mime.QEncoding.Encode("UTF-8", "网关服务巡检通知")
	message := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\nMessage-ID: <%s@gateway.invalid>\r\n\r\n%s\r\n", c.SMTPFrom, strings.Join(c.SMTPTo, ", "), subject, time.Now().Format(time.RFC1123Z), event.ID, strings.ReplaceAll(event.Message, "\n", "\r\n"))
	if _, err = writer.Write([]byte(message)); err != nil {
		_ = writer.Close()
		return errors.New("SMTP 邮件提交中断，结果需核对")
	}
	if writer.Close() != nil {
		return errors.New("SMTP 邮件未确认接收，结果需核对")
	}
	// A successful DATA response is the acceptance boundary; QUIT failure must
	// not trigger a duplicate message after the server has already accepted it.
	_ = client.Quit()
	return nil
}
