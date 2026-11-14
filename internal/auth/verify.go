package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"gorm.io/gorm"

	"github.com/linuxsuren/message-hub/internal/model"
)

// ErrCodeTooFrequent 验证码发送过于频繁。
var ErrCodeTooFrequent = errors.New("发送过于频繁，请 1 分钟后再试")

// codeTTL 验证码有效期。
const codeTTL = 10 * time.Minute

// IssueVerificationCode 为邮箱生成 6 位数字验证码（60 秒内不可重复发送）。
// 删除旧码与写入新码在同一事务内完成，避免并发请求双发。
func IssueVerificationCode(db *gorm.DB, email string) (string, error) {
	var code string
	err := db.Transaction(func(tx *gorm.DB) error {
		var recent model.VerifyCode
		err := tx.Where("email = ? AND created_at > ?", email, time.Now().Add(-time.Minute)).
			First(&recent).Error
		if err == nil {
			return ErrCodeTooFrequent
		}
		if err != gorm.ErrRecordNotFound {
			return err
		}
		n, err := rand.Int(rand.Reader, big.NewInt(1000000))
		if err != nil {
			return err
		}
		code = fmt.Sprintf("%06d", n.Int64())
		now := time.Now()
		if err := tx.Where("email = ?", email).Delete(&model.VerifyCode{}).Error; err != nil {
			return err
		}
		return tx.Create(&model.VerifyCode{
			Email: email, CodeHash: HashToken(code), ExpiresAt: now.Add(codeTTL), CreatedAt: now,
		}).Error
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// CheckVerificationCode 校验并消费验证码。
// 错误尝试会被计数：连续失败达到 model.MaxCodeAttempts 次后，验证码立即作废，
// 即使随后提交正确验证码也会被拒绝（防在线穷举）。
func CheckVerificationCode(db *gorm.DB, email, code string) bool {
	var rec model.VerifyCode
	err := db.Where("email = ? AND code_hash = ? AND used_at IS NULL AND expires_at > ?",
		email, HashToken(code), time.Now()).First(&rec).Error
	if err != nil {
		// 记录一次失败尝试；达到上限则顺带作废（used_at 置位）。
		db.Model(&model.VerifyCode{}).
			Where("email = ? AND used_at IS NULL AND expires_at > ?", email, time.Now()).
			Updates(map[string]any{
				"failed_attempts": gorm.Expr("failed_attempts + 1"),
				"used_at":         gorm.Expr("CASE WHEN failed_attempts + 1 >= ? THEN ? ELSE used_at END", model.MaxCodeAttempts, time.Now()),
			})
		return false
	}
	now := time.Now()
	return db.Model(&rec).Update("used_at", now).Error == nil
}
