package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	lxcConfig "github.com/clever-vpn/clever-vpn-lxc/config"
)

func backupToR2() error {
	bc := lxcConfig.Cfg.Backup
	if !bc.Enabled {
		return fmt.Errorf("backup not enabled in config")
	}
	if bc.R2Endpoint == "" || bc.R2Bucket == "" {
		return fmt.Errorf("r2_endpoint and r2_bucket required")
	}

	// Resolve credentials, allowing env var substitution
	accessKey := lxcConfig.ResolveEnv(bc.R2AccessKeyID)
	secretKey := lxcConfig.ResolveEnv(bc.R2SecretAccessKey)

	if accessKey == "" {
		accessKey = os.Getenv("R2_ACCESS_KEY_ID")
	}
	if secretKey == "" {
		secretKey = os.Getenv("R2_SECRET_ACCESS_KEY")
	}

	cred := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

	resolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:               bc.R2Endpoint,
			HostnameImmutable: true,
			SigningRegion:     "auto",
			Source:            aws.EndpointSourceCustom,
		}, nil
	})

	cfg, err := awsConfig.LoadDefaultConfig(context.Background(),
		awsConfig.WithCredentialsProvider(cred),
		awsConfig.WithEndpointResolverWithOptions(resolver),
		awsConfig.WithRegion("auto"),
	)
	if err != nil {
		return fmt.Errorf("s3 config: %w", err)
	}

	client := s3.NewFromConfig(cfg)

	dataDir := lxcConfig.EnsureDataDir()
	count := 0
	err = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// Skip certs directory
		if strings.Contains(path, "/certs/") {
			return nil
		}

		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(bc.R2Bucket),
			Key:    aws.String(rel),
			Body:   f,
		})
		if err != nil {
			return fmt.Errorf("put %s: %w", rel, err)
		}
		count++
		return nil
	})

	if err != nil {
		return err
	}
	log.Printf("Backup: %d files synced to %s/%s", count, bc.R2Endpoint, bc.R2Bucket)
	return nil
}

func restoreFromR2() error {
	bc := lxcConfig.Cfg.Backup
	if !bc.Enabled {
		return fmt.Errorf("backup not enabled in config")
	}
	if bc.R2Endpoint == "" || bc.R2Bucket == "" {
		return fmt.Errorf("r2_endpoint and r2_bucket required")
	}

	accessKey := lxcConfig.ResolveEnv(bc.R2AccessKeyID)
	secretKey := lxcConfig.ResolveEnv(bc.R2SecretAccessKey)
	if accessKey == "" {
		accessKey = os.Getenv("R2_ACCESS_KEY_ID")
	}
	if secretKey == "" {
		secretKey = os.Getenv("R2_SECRET_ACCESS_KEY")
	}

	cred := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

	resolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:               bc.R2Endpoint,
			HostnameImmutable: true,
			SigningRegion:     "auto",
			Source:            aws.EndpointSourceCustom,
		}, nil
	})

	cfg, err := awsConfig.LoadDefaultConfig(context.Background(),
		awsConfig.WithCredentialsProvider(cred),
		awsConfig.WithEndpointResolverWithOptions(resolver),
		awsConfig.WithRegion("auto"),
	)
	if err != nil {
		return fmt.Errorf("s3 config: %w", err)
	}

	client := s3.NewFromConfig(cfg)
	dataDir := lxcConfig.EnsureDataDir()

	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bc.R2Bucket),
	})

	count := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)

			// Skip certs
			if strings.HasPrefix(key, "certs/") {
				continue
			}

			localPath := filepath.Join(dataDir, key)
			os.MkdirAll(filepath.Dir(localPath), 0700)

			out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
				Bucket: aws.String(bc.R2Bucket),
				Key:    aws.String(key),
			})
			if err != nil {
				return fmt.Errorf("get %s: %w", key, err)
			}

			f, err := os.Create(localPath)
			if err != nil {
				out.Body.Close()
				return fmt.Errorf("create %s: %w", localPath, err)
			}
			_, err = io.Copy(f, out.Body)
			out.Body.Close()
			f.Close()
			if err != nil {
				return fmt.Errorf("write %s: %w", key, err)
			}
			count++
		}
	}

	log.Printf("Restore: %d files restored from %s/%s", count, bc.R2Endpoint, bc.R2Bucket)
	return nil
}

// startBackupLoop runs periodic backups in the background.
// Deprecated: replaced by startSyncLoop for event-driven sync.
func startBackupLoop() {
	if !lxcConfig.Cfg.Backup.Enabled {
		return
	}

	d, err := time.ParseDuration(lxcConfig.Cfg.Backup.Interval)
	if err != nil {
		log.Printf("Backup: invalid interval %q: %v", lxcConfig.Cfg.Backup.Interval, err)
		return
	}

	log.Printf("Backup: enabled, every %s", d)
	go func() {
		time.Sleep(30 * time.Second)
		for {
			if err := backupToR2(); err != nil {
				log.Printf("Backup: %v", err)
			}
			time.Sleep(d)
		}
	}()
}

// ==================== Event-driven sync ====================

// syncChan receives filenames that need syncing to R2.
var syncChan = make(chan string, 16)

// triggerSync signals that a file has changed and should be synced to R2.
func triggerSync(filename string) {
	select {
	case syncChan <- filename:
	default:
		// channel full, drop (next sync will pick up latest state)
	}
}

// syncFileToR2 uploads a single file to R2.
func syncFileToR2(filename string) error {
	bc := lxcConfig.Cfg.Backup
	if !bc.Enabled {
		return nil
	}

	accessKey := lxcConfig.ResolveEnv(bc.R2AccessKeyID)
	secretKey := lxcConfig.ResolveEnv(bc.R2SecretAccessKey)
	if accessKey == "" {
		accessKey = os.Getenv("R2_ACCESS_KEY_ID")
	}
	if secretKey == "" {
		secretKey = os.Getenv("R2_SECRET_ACCESS_KEY")
	}

	cred := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

	resolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:               bc.R2Endpoint,
			HostnameImmutable: true,
			SigningRegion:     "auto",
			Source:            aws.EndpointSourceCustom,
		}, nil
	})

	awscfg, err := awsConfig.LoadDefaultConfig(context.Background(),
		awsConfig.WithCredentialsProvider(cred),
		awsConfig.WithEndpointResolverWithOptions(resolver),
		awsConfig.WithRegion("auto"),
	)
	if err != nil {
		return fmt.Errorf("s3 config: %w", err)
	}

	client := s3.NewFromConfig(awscfg)

	dataDir := lxcConfig.EnsureDataDir()
	localPath := filepath.Join(dataDir, filename)

	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", filename, err)
	}

	_, err = client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bc.R2Bucket),
		Key:    aws.String(filename),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", filename, err)
	}

	log.Printf("Sync: %s synced to R2", filename)
	return nil
}

// startSyncLoop runs event-driven R2 sync with debounce.
// When a file changes, it waits for a quiet period (no more changes to the same file
// for syncDebounce) before uploading to R2.
func startSyncLoop() {
	if !lxcConfig.Cfg.Backup.Enabled {
		return
	}

	const syncDebounce = 60 * time.Second

	log.Printf("Sync: event-driven, debounce %s", syncDebounce)

	go func() {
		pending := map[string]time.Time{}

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case filename := <-syncChan:
				pending[filename] = time.Now()

			case <-ticker.C:
				now := time.Now()
				for f, t := range pending {
					if now.Sub(t) >= syncDebounce {
						if err := syncFileToR2(f); err != nil {
							log.Printf("Sync: %v", err)
						}
						delete(pending, f)
					}
				}
			}
		}
	}()
}
