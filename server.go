package main

import (
	"container/list"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Queue of to be converted filenames with a mutex for async access
type Queue struct {
	current string
	M       sync.Mutex
	list.List
}

var (
	pause, exit  chan bool
	threads, dir string
)

func init() {
	// flags
	threadsInt := flag.Int("threads", 2, "number of threads (between 1 and 10), used to throttle cpu usage")
	flag.StringVar(&dir, "tmpdir", "", "use this as a temporary directory for the converted file, in case the is nog diskspace left on the original files drive")
	flag.Parse()

	if *threadsInt < 1 || *threadsInt > 10 {
		log.Fatal("number of threads must be between 1 and 10")
	}
	threads = strconv.FormatInt(int64(*threadsInt), 10)

	// Setup signal handeling
	exit = make(chan bool, 1)
	pause = make(chan bool, 1)
	signalChan := make(chan os.Signal, 2)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		for {
			<-signalChan
			handleExit()
		}
	}()
	logFile, err := os.OpenFile("/var/log/mass-compress.log", syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT, 0666)
	if err != nil {
		log.Fatal("could not open logfile: %s", err.Error())
	}
	log.SetOutput(logFile)
}

func main() {
	// start goroutine handeling emptying the queue
	q := &Queue{}
	go compress(q, exit)

	// setting up everthing needed for the tcp connection
	ln, err := net.Listen("tcp", ":1234")
	if err != nil {
		log.Fatal("[*] could not liston on port 1234:", err)
	}
	defer ln.Close()
	log.Println("[-] Listening for connections on port 1234")
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Print("[*] error on accepting connection: ", err)
		}
		go handleConn(conn, q)
	}
}

func handleExit() {
	select {
	case exit <- true:
		log.Println("[-] shutting down gracefully, waiting for currently converted file to finish")
	default: // already send 1 interupt for graceful shutdown, (so exit chan will block)force it a second time
		log.Println("[+] shutting down forcefully, after receiving second request")
		os.Exit(0)
	}
}

func handleConn(c net.Conn, q *Queue) {
	defer c.Close()
	buff, err := ioutil.ReadAll(c)
	if err != nil {
		log.Print("error on reading from connection:", err)
	}
	filenames := strings.Split(strings.Trim(string(buff), "\n "), "\n")
	if len(filenames) == 1 && strings.HasPrefix(filenames[0], "--") {
		stripped := strings.TrimLeft(filenames[0], "--")
		command := strings.Split(stripped, " ")
		switch {
		case command[0] == "list":
			c.Write([]byte(fmt.Sprintf("working on: %s\n", q.current)))
			for e := q.Front(); e != nil; e = e.Next() {
				c.Write([]byte(fmt.Sprintf("%s\n", e.Value.(string))))
			}
		case command[0] == "pause":
			log.Println("[-] got pause message from client")
			c.Write([]byte("received pause command\n"))
			pause <- true
		case command[0] == "resume":
			log.Println("[+] got resume message from client")
			c.Write([]byte("received resume command\n"))
			pause <- false
		case command[0] == "stop":
			handleExit()
		case command[0] == "threads":
			if _, err := strconv.ParseInt(command[1], 10, 32); err == nil {
				threads = command[1]
				log.Printf("[+] setting number of threads to %s\n", threads)
				c.Write([]byte(fmt.Sprintf("setting number of threads to %s\n", threads)))
			} else {
				c.Write([]byte("[*] number of threads must be an integer\n"))
			}
		case command[0] == "clear":
			for q.Len() > 0 {
				q.Remove(q.Front())
			}
		}
		return
	}
	q.M.Lock()
files:
	for _, f := range filenames {
		for e := q.Front(); e != nil; e = e.Next() {
			if e.Value == f || f == q.current {
				c.Write([]byte(fmt.Sprintf("already in queue: %s\n", f)))
				continue files
			}
		}
		q.PushBack(f)
		c.Write([]byte(fmt.Sprintf("added to queue: %s\n", f)))
	}
	q.M.Unlock()
}

func compress(q *Queue, exit chan bool) {
	for {
		select {
		case <-exit:
			os.Exit(0)
		default:
			if q.Len() > 0 {
				q.M.Lock()
				item := q.Remove(q.Front())
				filename := item.(string)
				q.current = filename
				q.M.Unlock()
				var stat os.FileInfo
				var err error
				if stat, err = os.Stat(filename); os.IsNotExist(err) {
					log.Printf("%s does not exist (anymore), skipping\n", filename)
					continue
				}
				isoregex := regexp.MustCompile("(?i:iso|img)$")
				var newfile string
				if isoregex.MatchString(filename) {
					newfile, err = convertIso(filename)
				} else if stat.IsDir() {
					newfile, err = handleDVDFolder(filename)
				} else {
					newfile, err = convertVideo(filename)
				}
				if err != nil {
					fmt.Println(err)
					continue
				}
				err = os.RemoveAll(filename)
				if err != nil {
					fmt.Println(prefixError("failed to remove "+filename, err))
					continue
				}
				log.Printf("[+] %s compressed and old one deleted\n", filename)
				// if using a tempdir for compressed file, move it back to location of source file
				if dir != "" {
					dest := filepath.Join(filepath.Dir(filename), filepath.Base(newfile))
					log.Printf("[-] copying %s -> %s", newfile, dest)
					if _, err := copy(newfile, dest); err != nil {
						log.Println(prefixError("copying to final destination", err))
						continue
					}
					log.Println("[+] copy completed")
					err = os.Remove(newfile)
					if err != nil {
						log.Println(prefixError("removing tmp file", err))
						continue
					}
					newfile = dest
				}
				err = os.Chown(newfile, 1000, 1000) // uid of fkalter
				if err != nil {
					err = prefixError("chowning: ", err)
					return
				}
				q.current = ""
			}
		}
		// if queue is empty, the neverending for loop wil run amok
		time.Sleep(1 * time.Second)
	}
}

func handleDVDFolder(filename string) (newfile string, err error) {
	input, err := getVideoTSInput(filename)
	if err != nil {
		return newfile, err
	}
	if dir == "" {
		newfile = filepath.Dir(filename)
	} else {
		newfile = dir
	}
	resolution := "720x480"
	newfile = filepath.Join(newfile, "compressed.mp4")
	cmd := exec.Command("ffmpeg", "-i", input,
		"-sn",                             // disable subtitles
		"-c:v", "libx264", "-vf", "yadif", // x264 video codec, video filter to deinterlace video
		"-crf", "27", // constant rate factor, compromise between quality and size
		"-s", resolution, // set output resolution
		"-c:a", "copy", // just copy the audio, no de/encoding
		"-threads", threads, "-y", newfile) // 2 threads to throttle cpu usage, -y to overwrite output file
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Start()
	if err != nil {
		err = prefixError("compressing: ", err)
		return
	}
	err = processWait(cmd)
	if err != nil {
		return
	}
	return newfile, nil
}

func getVideoTSInput(filename string) (newfile string, err error) {
	vobs, err := findMainMovie(filename)
	if err != nil {
		err = prefixError("globbing:", err)
		return newfile, err
	}
	newfile = "concat:"
	for i, f := range vobs {
		if i == len(vobs)-1 {
			newfile = fmt.Sprintf("%s%s", newfile, f)
		} else {
			newfile = fmt.Sprintf("%s%s|", newfile, f)
		}
	}
	return
}

func copy(src, dst string) (bytes int64, err error) {
	srcFile, err := os.Open(src)
	if err != nil {
		return
	}
	defer srcFile.Close()

	srcFileStat, err := srcFile.Stat()
	if err != nil {
		return
	}

	if !srcFileStat.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", src)
		return
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		return
	}
	defer dstFile.Close()
	return io.Copy(dstFile, srcFile)
}

func prefixError(prefix string, err error) error {
	return fmt.Errorf("[*] %s: %s", prefix, err)
}

func findBestResolution(filename string) string {
	res := "1280x720"
	cmd := exec.Command("ffmpeg", "-i", filename)
	out, _ := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, ": Video:") {
			re := regexp.MustCompile("([0-9]{2,5})x[0-9]{2,5}")
			horizontal, _ := strconv.ParseInt(re.FindStringSubmatch(line)[1], 10, 64)
			if horizontal < 1280 {
				res = re.FindStringSubmatch(line)[0]
			}
		}
	}
	return res
}

func convertIso(filename string) (newfile string, err error) {
	mountPoint := "/media/film"
	cmd := exec.Command("sudo", "umount", mountPoint)
	err = cmd.Run()
	cmd = exec.Command("sudo", "mount", filename, mountPoint)
	err = cmd.Run()
	if err != nil {
		err = prefixError("error mounting %s: %s", err)
		return
	}

	var input, resolution string
	if _, err := os.Stat(filepath.Join(mountPoint, "VIDEO_TS")); !os.IsNotExist(err) {
		vobs, err := findMainMovie(filepath.Join(mountPoint, "VIDEO_TS"))
		if err != nil {
			err = prefixError("globbing:", err)
			return newfile, err
		}
		input = "concat:"
		for i, f := range vobs {
			if i == len(vobs)-1 {
				input = fmt.Sprintf("%s%s", input, f)
			} else {
				input = fmt.Sprintf("%s%s|", input, f)
			}
		}
		resolution = findBestResolution(vobs[0])
	} else if _, err := os.Stat(filepath.Join(mountPoint, "BDMW")); !os.IsNotExist(err) {
		media, _ := filepath.Glob(filepath.Join(mountPoint, "BDWM", "*"))
		var size int64
		for _, f := range media {
			f = filepath.Join(mountPoint, "BDWM", f)
			if stat, err := os.Stat(f); err == nil {
				if s := stat.Size(); s > size {
					size, input = s, f
				}
			}
		}
		resolution = findBestResolution(input)
	} else {
		return "", errors.New("unknown image format")
	}
	if dir == "" {
		newfile = filepath.Dir(filename)
	} else {
		newfile = dir
	}
	newfile = filepath.Join(newfile, "compressed.mp4")
	cmd = exec.Command("ffmpeg", "-i", input,
		"-sn",                             // disable subtitles
		"-c:v", "libx264", "-vf", "yadif", // x264 video codec, video filter to deinterlace video
		"-crf", "27", // constant rate factor, compromise between quality and size
		"-s", resolution, // set output resolution
		"-c:a", "copy", // just copy the audio, no de/encoding
		"-threads", threads, "-y", newfile) // 2 threads to throttle cpu usage, -y to overwrite output file
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Start()
	if err != nil {
		err = prefixError("compressing: ", err)
		return
	}
	err = processWait(cmd)
	if err != nil {
		return
	}

	cmd = exec.Command("sudo", "umount", "/media/film")
	err = cmd.Run()
	if err != nil {
		err = prefixError("unmounting: ", err)
		return
	}
	return
}

func findMainMovie(videoTs string) (vobs []string, err error) {
	// build map of major vts numbers -> total size of these files
	// the major number with the biggest combined size is probably the main movie
	files, _ := filepath.Glob(filepath.Join(videoTs, "*.VOB"))
	sizeMap := make(map[string]int64)
	majorRegex := regexp.MustCompile("VTS_([0-9]+)_[0-9]+.VOB")
	for _, f := range files {
		if !majorRegex.MatchString(f) {
			continue
		}
		major := majorRegex.FindStringSubmatch(f)[1]
		stat, err := os.Stat(f)
		if err != nil {
			return vobs, err
		}
		sizeMap[major] = sizeMap[major] + stat.Size()
	}

	// determine the biggest major from the map
	var max int64
	var major string
	for m, s := range sizeMap {
		if s > max {
			max, major = s, m
		}
	}

	vobs = make([]string, 0)
	// return every vob file with the found major number and a minor number bigger than 0
	mainMovieRegex := regexp.MustCompile(fmt.Sprintf("VTS_%s_([0-9]{2}|[1-9]).VOB", major))
	for _, v := range files {
		if mainMovieRegex.MatchString(v) {
			vobs = append(vobs, v)
		}
	}
	return
}

func processWait(cmd *exec.Cmd) error {
	done := make(chan error)
	log.Println("[-] starting: ", cmd.Path, cmd.Args)
	go func() {
		done <- cmd.Wait()
	}()
selectloop:
	for {
		select {
		case p := <-pause:
			if p {
				cmd.Process.Signal(syscall.SIGSTOP)
			} else {
				cmd.Process.Signal(syscall.SIGCONT)
			}
		case err := <-done:
			if err != nil {
				return prefixError(cmd.Path+" return code:", err)
			}
			log.Println("[+] completed: ", cmd.Path, cmd.Args)
			break selectloop
		default:
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

func convertVideo(filename string) (newfile string, err error) {
	resolution := findBestResolution(filename)
	if dir == "" {
		newfile = filepath.Dir(filename)
	} else {
		newfile = dir
	}
	newfile = filepath.Join(newfile, fmt.Sprintf("%s-compressed.mp4",
		strings.Replace(filepath.Base(filename), filepath.Ext(filename), "", -1)))

	audio := "copy"
	if strings.HasSuffix(filename, "wmv") {
		audio = "libvorbis"
	}
	cmd := exec.Command("ffmpeg", "-i", filename,
		"-sn",                             // disable subtitles
		"-c:v", "libx264", "-vf", "yadif", // x264 video codec, video filter to deinterlace video
		"-crf", "27", // constant rate factor, compromise between quality and size
		"-s", resolution, // set output resolution
		"-c:a", audio, // just copy the audio, no de/encoding
		"-threads", threads, "-y", newfile) // 2 threads to throttle cpu usage, -y to overwrite output file
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Start()
	if err != nil {
		err = prefixError("compressing: ", err)
		return
	}
	err = processWait(cmd)
	if err != nil {
		return
	}
	return
}
