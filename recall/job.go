package recall

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cdr/amboy"
	"github.com/cdr/amboy/dependency"
	"github.com/cdr/amboy/job"
	"github.com/cdr/amboy/registry"
	"github.com/deciduosity/bond"
	"github.com/cdr/grip"
	"github.com/cdr/grip/message"
	"github.com/mholt/archiver"
	"github.com/pkg/errors"
)

// DownloadFileJob is an amboy.Job implementation that supports
// downloading a a file to the local file system.
type DownloadFileJob struct {
	URL       string `bson:"url" json:"url" yaml:"url"`
	Directory string `bson:"dir" json:"dir" yaml:"dir"`
	FileName  string `bson:"file" json:"file" yaml:"file"`
	*job.Base `bson:"metadata" json:"metadata" yaml:"metadata"`
}

func init() {
	registry.AddJobType("bond-recall-download-file", func() amboy.Job {
		return newDownloadJob()
	})
}

func newDownloadJob() *DownloadFileJob {
	return &DownloadFileJob{
		Base: &job.Base{
			JobType: amboy.JobType{
				Name:    "bond-recall-download-file",
				Version: 0,
			},
		},
	}
}

// NewDownloadJob constructs a DownloadFileJob. The job has a
// dependency on the downloaded file, and will only execute if that
// file does not exist.
func NewDownloadJob(url, path string, force bool) (*DownloadFileJob, error) {
	j := newDownloadJob()
	if err := j.setURL(url); err != nil {
		return nil, errors.Wrap(err, "problem constructing Job object (url)")
	}

	if err := j.setDirectory(path); err != nil {
		return nil, errors.Wrap(err, "problem constructing Job object (directory)")
	}

	fn := j.getFileName()
	j.SetID(fmt.Sprintf("%s-%d",
		strings.Replace(fn, string(filepath.Separator), "-", -1),
		job.GetNumber()))

	if force || strings.Contains(fn, "latest") {
		_ = os.Remove(fn)
		_ = os.RemoveAll(fn[:len(fn)-4])
		j.SetDependency(dependency.NewAlways())
	} else {
		j.SetDependency(dependency.NewCreatesFile(fn))
	}

	return j, nil
}

// Run implements the main action of the Job. This implementation
// checks the job directly and returns early if the downloaded file
// exists. This behavior may be redundant in the case that the queue
// skips jobs with "passed" jobs.
func (j *DownloadFileJob) Run(ctx context.Context) {
	defer j.MarkComplete()

	fn := j.getFileName()
	defer attemptTimestampUpdate(fn)

	// in theory the queue should do this next check, but most do not
	if state := j.Dependency().State(); state == dependency.Passed {
		grip.Debug(message.Fields{
			"file":    fn,
			"message": "file is already downloaded",
			"op":      "none",
		})
		return
	}

	if err := bond.DownloadFile(ctx, j.URL, fn); err != nil {
		j.handleError(errors.Wrapf(err, "problem downloading file %s", fn))
		return
	}

	grip.Debug(message.Fields{
		"op":   "downloaded file complete",
		"file": fn,
	})

	if err := extractArchive(fn); err != nil {
		j.handleError(errors.Wrap(err, "problem extracting artifacts"))
		return
	}
}

//
// Internal Methods
//

func extractArchive(fn string) error {
	return errors.WithStack(archiver.Unarchive(fn, getTargetDirectory(fn)))
}

func getTargetDirectory(fn string) string {
	baseName := filepath.Base(fn)
	return filepath.Join(filepath.Dir(fn), baseName[:len(baseName)-len(filepath.Ext(baseName))])
}

func attemptTimestampUpdate(fn string) {
	// update the timestamps so we playwell with the cache. These
	// operations are logged but don't impact the tasks error
	// state if they fail.
	now := time.Now()
	if err := os.Chtimes(fn, now, now); err != nil {
		grip.Debug(err)
	}

	// hopefully directory names in archives are the same are the
	// same as the filenames. Unwinding this assumption will
	// probably require a different archiver tool.
	dirname := fn[0 : len(fn)-len(filepath.Ext(fn))]
	if err := os.Chtimes(dirname, now, now); err != nil {
		grip.Debug(err)
	}
}

func (j *DownloadFileJob) handleError(err error) {
	j.AddError(err)

	grip.Error(message.WrapError(err, message.Fields{
		"message": "problem downloading file",
		"name":    j.FileName,
		"op":      "cleaning up artifacts",
	}))
	grip.Warning(os.RemoveAll(j.getFileName())) // cleanup
}

func (j *DownloadFileJob) getFileName() string {
	return filepath.Join(j.Directory, j.FileName)
}

func (j *DownloadFileJob) setDirectory(path string) error {
	if stat, err := os.Stat(path); !os.IsNotExist(err) && !stat.IsDir() {
		// if the path exists and isn't a directory, then we
		// won't be able to download into it:
		return errors.Errorf("%s is not a directory, cannot download files into it",
			path)
	}

	j.Directory = path
	return nil
}

func (j *DownloadFileJob) setURL(url string) error {
	if !strings.HasPrefix(url, "http") {
		return errors.Errorf("%s is not a valid url", url)
	}

	if strings.HasSuffix(url, "/") {
		return errors.Errorf("%s does not contain a valid filename component", url)
	}

	j.URL = url
	j.FileName = filepath.Base(url)

	if strings.HasSuffix(url, ".tar.gz") {
		j.FileName = filepath.Ext(filepath.Ext(j.FileName)) + ".tgz"
	}

	return nil
}
