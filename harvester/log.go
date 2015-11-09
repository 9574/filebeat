package harvester

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/elastic/filebeat/config"
	"github.com/elastic/filebeat/input"
	"github.com/elastic/libbeat/logp"
)

func NewHarvester(
	prospectorCfg config.ProspectorConfig,
	cfg *config.HarvesterConfig,
	path string,
	signal chan int64,
	spooler chan *input.FileEvent,
) (*Harvester, error) {
	encoding, ok := findEncoding(cfg.Encoding)
	if !ok || encoding == nil {
		return nil, fmt.Errorf("unknown encoding('%v')", cfg.Encoding)
	}

	h := &Harvester{
		Path:             path,
		ProspectorConfig: prospectorCfg,
		Config:           cfg,
		FinishChan:       signal,
		SpoolerChan:      spooler,
		encoding:         encoding,
		backoff:          prospectorCfg.Harvester.BackoffDuration,
	}
	return h, nil
}

// Log harvester reads files line by line and sends events to the defined output
func (h *Harvester) Harvest() {

	err := h.open()

	// Make sure file is closed as soon as harvester exits
	defer h.file.Close()

	if err != nil {
		logp.Err("Stop Harvesting. Unexpected Error: %s", err)
		return
	}

	info, err := h.file.Stat()
	if err != nil {
		logp.Err("Stop Harvesting. Unexpected Error: %s", err)
		return
	}

	// On completion, push offset so we can continue where we left off if we relaunch on the same file
	defer func() {
		h.FinishChan <- h.Offset
	}()

	var line uint64 = 0

	// Load last offset from registrar
	h.initOffset()

	in := h.encoding(h.file)

	reader := bufio.NewReaderSize(in, h.Config.BufferSize)
	buffer := bytes.NewBuffer(nil)
	hConfig := h.ProspectorConfig.Harvester

	lastReadTime := time.Now()

	for {
		text, err := readLine(reader, buffer, hConfig.PartialLineWaitingDuration)

		if err != nil {

			// In case of only err = io.EOF returns nil
			err = h.handleReadlineError(lastReadTime, err)
			if err != nil {
				logp.Err("File reading error. Stopping harvester. Error: %s", err)
				return
			}

			err = h.handleEndOfFile()
			if err != nil {
				logp.Err("End of file. Stopping harvester. Error: %s", err)
				return
			}

			// EOF reached
			// Encoding and reader are reinitialised here as other encoder stops reading. See #182
			in = h.encoding(h.file)
			reader.Reset(in)
			continue
		}

		lastReadTime = time.Now()
		h.backoff = hConfig.BackoffDuration
		line++

		// Sends text to spooler
		event := &input.FileEvent{
			ReadTime:     lastReadTime,
			Source:       &h.Path,
			InputType:    h.Config.InputType,
			DocumentType: h.Config.DocumentType,
			Offset:       h.Offset,
			Line:         line,
			Text:         text,
			Fields:       &h.Config.Fields,
			Fileinfo:     &info,
		}

		event.SetFieldsUnderRoot(h.Config.FieldsUnderRoot)

		h.Offset, err = h.file.Seek(0, os.SEEK_CUR) // Update offset
		if err != nil {
			logp.Err("Error getting the current offset: %v. Stopping harverster", err)
			return
		}

		h.SpoolerChan <- event // ship the new event downstream
	}
}

// Handles the end of a file.
// Introduces a backoff wait if needed
// Returns error in case file should be closed
func (h *Harvester) handleEndOfFile() error {

	config := h.ProspectorConfig.Harvester

	// On windows, check if the file name exists (see #93)
	if config.ForceCloseWindowsFiles && runtime.GOOS == "windows" {
		_, statErr := os.Stat(h.file.Name())
		if statErr != nil {
			logp.Err("Unexpected windows specific error reading from %s; error: %s", h.Path, statErr)
			// Return directly on windows -> file is closing
			return statErr
		}
	}

	// Wait before trying to read file which reached EOF again
	time.Sleep(h.backoff)

	// Increment backoff up to maxBackoff
	if h.backoff < config.MaxBackoffDuration {
		h.backoff = h.backoff * time.Duration(config.BackoffFactor)
		if h.backoff > config.MaxBackoffDuration {
			h.backoff = config.MaxBackoffDuration
		}
	}

	return nil
}

// initOffset finds the current offset of the file and sets it in the harvester as position
func (h *Harvester) initOffset() {
	// get current offset in file
	offset, _ := h.file.Seek(0, os.SEEK_CUR)

	if h.Offset > 0 {
		logp.Debug("harvester", "harvest: %q position:%d (offset snapshot:%d)", h.Path, h.Offset, offset)
	} else if h.Config.TailFiles {
		logp.Debug("harvester", "harvest: (tailing) %q (offset snapshot:%d)", h.Path, offset)
	} else {
		logp.Debug("harvester", "harvest: %q (offset snapshot:%d)", h.Path, offset)
	}

	h.Offset = offset
}

// Sets the offset of the file to the right place. Takes configuration options into account
func (h *Harvester) setFileOffset() {
	if h.Offset > 0 {
		h.file.Seek(h.Offset, os.SEEK_SET)
	} else if h.Config.TailFiles {
		h.file.Seek(0, os.SEEK_END)
	} else {
		h.file.Seek(0, os.SEEK_SET)
	}
}

// open does open the file given under h.Path and assigns the file handler to h.file
func (h *Harvester) open() error {
	// Special handling that "-" means to read from standard input
	if h.Path == "-" {
		h.file = os.Stdin
		return nil
	}

	for {
		var err error
		h.file, err = input.ReadOpen(h.Path)

		if err != nil {
			// TODO: This is currently end endless retry, should be set to a max?
			// retry on failure.
			logp.Err("Failed opening %s: %s", h.Path, err)
			time.Sleep(5 * time.Second)
		} else {
			break
		}
	}

	file := &input.File{
		File: h.file,
	}

	// Check we are not following a rabbit hole (symlinks, etc.)
	if !file.IsRegularFile() {
		return errors.New("Given file is not a regular file.")
	}

	h.setFileOffset()

	return nil
}

// handleReadlineError handles error which are raised during reading file.
//
// If error is EOF, it will check for:
// * File truncated
// * Older then ignore_older
// * General file error
//
// If none of the above cases match, no error will be returned and file is kept open
//
// In case of a general error, the error itself is returned
func (h *Harvester) handleReadlineError(lastTimeRead time.Time, err error) error {

	if err == io.EOF {
		// Refetch fileinfo to check if the file was truncated or disappeared
		info, statErr := h.file.Stat()

		// This could happen if the file was removed / rotate after reading and before calling the stat function
		if statErr != nil {
			logp.Err("Unexpected error reading from %s; error: %s", h.Path, statErr)
			return statErr
		}

		// Check if file was truncated
		if info.Size() < h.Offset {
			logp.Debug("harvester", "File was truncated as offset (%s) > size (%s). Begin reading file from offset 0: %s", h.Offset, info.Size(), h.Path)
			h.Offset = 0
			h.file.Seek(h.Offset, os.SEEK_SET)
		} else if age := time.Since(lastTimeRead); age > h.ProspectorConfig.IgnoreOlderDuration {
			// If the file hasn't change for longer the ignore_older, harvester stops and file handle will be closed.
			logp.Debug("harvester", "Stopping harvesting of file as older then ignore_old: ", h.Path, "Last change was: ", age)
			return err
		}
		// Do nothing in case it is just EOF, keep reading the file
		return nil
	} else {
		logp.Err("Unexpected state reading from %s; error: %s", h.Path, err)
		return err
	}
}

func (h *Harvester) Stop() {
}

/*** Utility Functions ***/

// isLine checks if the given byte array is a line, means has a line ending \n
func isLine(line []byte) bool {
	if line == nil || len(line) == 0 {
		return false
	}

	if line[len(line)-1] != '\n' {
		return false
	}
	return true
}

// lineEndingChars returns the number of line ending chars the given by array has
// In case of Unix/Linux files, it is -1, in case of Windows mostly -2
func lineEndingChars(line []byte) int {
	if !isLine(line) {
		return 0
	}

	if line[len(line)-1] == '\n' {
		if len(line) > 1 && line[len(line)-2] == '\r' {
			return 2
		}

		return 1
	}
	return 0
}

// readLine reads a full line into buffer and returns it.
// In case of partial lines, readLine waits for a maximum of partialLineWaiting seconds for new segments to arrive.
// This could potentialy be improved / replaced by https://github.com/elastic/libbeat/tree/master/common/streambuf
func readLine(reader *bufio.Reader, buffer *bytes.Buffer, partialLineWaiting time.Duration) (*string, error) {

	lastSegementTime := time.Now()
	isPartialLine := true

	for {
		segment, err := reader.ReadBytes('\n')

		if segment != nil && len(segment) > 0 {
			if isLine(segment) {
				isPartialLine = false
			}

			// Update last segment time as new segment of line arrived
			lastSegementTime = time.Now()
			buffer.Write(segment)
		}

		if err != nil {
			// EOF, jump out of the loop
			if err == io.EOF {
				return nil, err
			}

			if isPartialLine {
				// Wait for a second for the next segments
				time.Sleep(1 * time.Second)

				// If last segment written is older then partialLineWaiting, partial line is discarded
				if time.Since(lastSegementTime) >= partialLineWaiting {
					return nil, err
				}
				continue
			} else {
				logp.Err("Error reading line: %s", err.Error())
				return nil, err
			}
		}

		// If we got a full line, return the whole line without the EOL chars (LF or CRLF)
		if !isPartialLine {

			str := buffer.String()

			// Get the str length with the EOL chars (LF or CRLF) and remove the last bytes
			str = str[:len(str)-lineEndingChars(segment)]
			// Reset the buffer for the next line
			buffer.Reset()

			return &str, nil
		}
	}
}
