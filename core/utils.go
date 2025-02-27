package core

import (
	"fmt"
	"os"
	"path"
	"time"

	"github.com/celestix/gotgproto/ext"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/tg"
	"github.com/krau/SaveAny-Bot/bot"
	"github.com/krau/SaveAny-Bot/common"
	"github.com/krau/SaveAny-Bot/config"
	"github.com/krau/SaveAny-Bot/logger"
	"github.com/krau/SaveAny-Bot/storage"
	"github.com/krau/SaveAny-Bot/types"
)

func saveFileWithRetry(task *types.Task, taskStorage storage.Storage, localFilePath string) error {
	for i := 0; i <= config.Cfg.Retry; i++ {
		if err := taskStorage.Save(task.Ctx, localFilePath, task.StoragePath); err != nil {
			if i == config.Cfg.Retry {
				return fmt.Errorf("failed to save file: %w", err)
			}
			logger.L.Errorf("Failed to save file: %s, retrying...", err)
			continue
		}
		return nil
	}
	return nil
}

func processPhoto(task *types.Task, taskStorage storage.Storage, cachePath string) error {
	res, err := bot.Client.API().UploadGetFile(task.Ctx, &tg.UploadGetFileRequest{
		Location: task.File.Location,
		Offset:   0,
		Limit:    1024 * 1024,
	})
	if err != nil {
		return fmt.Errorf("failed to get file: %w", err)
	}

	result, ok := res.(*tg.UploadFile)
	if !ok {
		return fmt.Errorf("unexpected type %T", res)
	}

	if err := os.WriteFile(cachePath, result.Bytes, os.ModePerm); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	defer cleanCacheFile(cachePath)

	logger.L.Infof("Downloaded file: %s", cachePath)

	return saveFileWithRetry(task, taskStorage, cachePath)
}

// func getProgressBar(progress float64, updateCount int) string {
// 	bar := ""
// 	barSize := 100 / updateCount
// 	for i := 0; i < updateCount; i++ {
// 		if progress >= float64(barSize*(i+1)) {
// 			bar += "█"
// 		} else {
// 			bar += "░"
// 		}
// 	}
// 	return bar
// }

func cleanCacheFile(destPath string) {
	if config.Cfg.Temp.CacheTTL > 0 {
		common.RmFileAfter(destPath, time.Duration(config.Cfg.Temp.CacheTTL)*time.Second)
	} else {
		if err := os.Remove(destPath); err != nil {
			logger.L.Errorf("Failed to purge file: %s", err)
		}
	}
}

// 获取进度需要更新的次数
func getProgressUpdateCount(fileSize int64) int {
	updateCount := 5
	if fileSize > 1024*1024*1000 {
		updateCount = 50
	} else if fileSize > 1024*1024*500 {
		updateCount = 20
	} else if fileSize > 1024*1024*200 {
		updateCount = 10
	}
	return updateCount
}

func getSpeed(bytesRead int64, startTime time.Time) string {
	if startTime.IsZero() {
		return "0MB/s"
	}
	elapsed := time.Since(startTime)
	speed := float64(bytesRead) / 1024 / 1024 / elapsed.Seconds()
	return fmt.Sprintf("%.2fMB/s", speed)
}

func buildProgressMessageEntity(task *types.Task, bytesRead int64, startTime time.Time, progress float64) (string, []tg.MessageEntityClass) {
	entityBuilder := entity.Builder{}
	text := fmt.Sprintf("正在处理下载任务\n文件名: %s\n保存路径: %s\n平均速度: %s\n当前进度: %.2f%%",
		task.FileName(),
		fmt.Sprintf("[%s]:%s", task.StorageName, task.StoragePath),
		getSpeed(bytesRead, startTime),
		progress,
	)
	var entities []tg.MessageEntityClass
	if err := styling.Perform(&entityBuilder,
		styling.Plain("正在处理下载任务\n文件名: "),
		styling.Code(task.FileName()),
		styling.Plain("\n保存路径: "),
		styling.Code(fmt.Sprintf("[%s]:%s", task.StorageName, task.StoragePath)),
		styling.Plain("\n平均速度: "),
		styling.Bold(getSpeed(bytesRead, task.StartTime)),
		styling.Plain("\n当前进度: "),
		styling.Bold(fmt.Sprintf("%.2f%%", progress)),
	); err != nil {
		logger.L.Errorf("Failed to build entities: %s", err)
		return text, entities
	}
	return entityBuilder.Complete()
}

func buildProgressCallback(ctx *ext.Context, task *types.Task, updateCount int) func(bytesRead, contentLength int64) {
	return func(bytesRead, contentLength int64) {
		progress := float64(bytesRead) / float64(contentLength) * 100
		logger.L.Tracef("Downloading %s: %.2f%%", task.String(), progress)
		progressInt := int(progress)
		if task.File.FileSize < 1024*1024*50 || progressInt == 0 || progressInt%int(100/updateCount) != 0 {
			return
		}
		text, entities := buildProgressMessageEntity(task, bytesRead, task.StartTime, progress)
		ctx.EditMessage(task.ReplyChatID, &tg.MessagesEditMessageRequest{
			Message:     text,
			Entities:    entities,
			ID:          task.ReplyMessageID,
			ReplyMarkup: getCancelTaskMarkup(task),
		})
	}
}

func getCancelTaskMarkup(task *types.Task) *tg.ReplyInlineMarkup {
	return &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonCallback{Text: "取消任务", Data: fmt.Appendf(nil, "cancel %s", task.Key())}}}},
	}
}

func fixTaskFileExt(task *types.Task, localFilePath string) {
	if path.Ext(task.FileName()) == "" {
		mimeType, err := mimetype.DetectFile(localFilePath)
		if err != nil {
			logger.L.Errorf("Failed to detect mime type: %s", err)
		} else {
			task.File.FileName = fmt.Sprintf("%s%s", task.FileName(), mimeType.Extension())
			task.StoragePath = fmt.Sprintf("%s%s", task.StoragePath, mimeType.Extension())
		}
	}
}

func getTaskThreads(fileSize int64) int {
	threads := 1
	if fileSize > 1024*1024*100 {
		threads = config.Cfg.Threads
	} else if fileSize > 1024*1024*50 {
		threads = config.Cfg.Threads / 2
	}
	return threads
}

type TaskLocalFile struct {
	file             *os.File
	size             int64
	done             int64
	progressCallback func(bytesRead, contentLength int64)
	callbackTimes    int64
	nextCallbackAt   int64
	callbackInterval int64
}

func (t *TaskLocalFile) Read(p []byte) (n int, err error) {
	return t.file.Read(p)
}

func (t *TaskLocalFile) Close() error {
	return t.file.Close()
}
func (t *TaskLocalFile) WriteAt(p []byte, off int64) (int, error) {
	n, err := t.file.WriteAt(p, off)
	if err != nil {
		return n, err
	}
	t.done += int64(n)
	if t.progressCallback != nil && t.done >= t.nextCallbackAt {
		t.progressCallback(t.done, t.size)
		t.nextCallbackAt += t.callbackInterval
	}
	return n, nil
}

func NewTaskLocalFile(filePath string, fileSize int64, progressCallback func(bytesRead, contentLength int64)) (*TaskLocalFile, error) {
	file, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	var callbackInterval int64
	callbackInterval = fileSize / 100
	if callbackInterval == 0 {
		callbackInterval = 1
	}
	return &TaskLocalFile{
		file:             file,
		size:             fileSize,
		progressCallback: progressCallback,
		callbackTimes:    100,
		nextCallbackAt:   callbackInterval,
		callbackInterval: callbackInterval,
	}, nil
}
