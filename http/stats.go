// Copyright (c) 2014-2017 Ludovic Fauvet
// Licensed under the MIT license

package http

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xbmc/mirrorbits/database"
	"github.com/xbmc/mirrorbits/filesystem"
	"github.com/xbmc/mirrorbits/mirrors"
	"github.com/xbmc/mirrorbits/useragent"
)

/*
	Total (all files, all mirrors):
	STATS_TOTAL

	List of hashes for a file:
	STATS_FILE							= path -> value		All time
	STATS_FILE_[year]					= path -> value		By year
	STATS_FILE_[year]_[month]			= path -> value		By month
	STATS_FILE_[year]_[month]_[day]		= path -> value		By day

	List of hashes for a mirror:
	STATS_MIRROR						= mirror -> value	All time
	STATS_MIRROR_[year]					= mirror -> value	By year
	STATS_MIRROR_[year]_[month]			= mirror -> value	By month
	STATS_MIRROR_[year]_[month]_[day]	= mirror -> value	By day
*/

var (
	emptyFileError = errors.New("stats: file parameter is empty")
	unknownMirror  = errors.New("stats: unknown mirror")
)

type Stats struct {
	r         *database.Redis
	countChan chan CountItem
	mapStats  map[string]int64
	stop      chan bool
	uaChan    chan useragent.UaInfo
	wg        sync.WaitGroup
}

type CountItem struct {
	mirrorID string
	filepath string
	size     int64
	time     time.Time
}

func NewStats(redis *database.Redis) *Stats {
	s := &Stats{
		r:         redis,
		countChan: make(chan CountItem, 1000),
		mapStats:  make(map[string]int64),
		stop:      make(chan bool),
		uaChan:    make(chan useragent.UaInfo, 1000),
	}
	go s.processCountDownload()
	return s
}

func (s *Stats) Terminate() {
	close(s.stop)
	log.Notice("Saving stats")
	s.wg.Wait()
}

// Lightweight method used to count a new download for a specific file and mirror
func (s *Stats) CountDownload(m mirrors.Mirror, fileinfo filesystem.FileInfo, uaInfo useragent.UaInfo) error {
	if m.ID == "" {
		return unknownMirror
	}
	if fileinfo.Path == "" {
		return emptyFileError
	}

	s.countChan <- CountItem{m.ID, fileinfo.Path, fileinfo.Size, time.Now().UTC()}
	s.uaChan <- uaInfo
	return nil
}

// Process all stacked download messages
func (s *Stats) processCountDownload() {
	s.wg.Add(1)
	pushTicker := time.NewTicker(500 * time.Millisecond)

	for {
		select {
		case <-s.stop:
			s.pushStats()
			s.wg.Done()
			return
		case c := <-s.countChan:
			date := c.time.Format("2006_01_02|") // Includes separator
			s.mapStats["f"+date+c.filepath] += 1
			s.mapStats["m"+date+c.mirrorID] += 1
			s.mapStats["s"+date+c.mirrorID] += c.size
		case c := <-s.uaChan:
			date := time.Now().Format("2006_01_02|") // Includes separator
			if c.Special {
				s.mapStats["P"+date+c.Platform] += 1
				s.mapStats["O"+date+c.OS] += 1
				s.mapStats["B"+date+c.Browser] += 1
			} else {
				s.mapStats["p"+date+c.Platform] += 1
				s.mapStats["o"+date+c.OS] += 1
				s.mapStats["b"+date+c.Browser] += 1
			}
		case <-pushTicker.C:
			s.pushStats()
		}
	}
}

// Push the resulting stats on redis
func (s *Stats) pushStats() {
	if len(s.mapStats) <= 0 {
		return
	}

	rconn := s.r.Get()
	defer rconn.Close()
	rconn.Send("MULTI")

	for k, v := range s.mapStats {
		if v == 0 {
			continue
		}

		separator := strings.Index(k, "|")
		if separator <= 0 {
			log.Critical("Stats: separator not found")
			continue
		}
		typ := k[:1]
		date := k[1:separator]
		object := k[separator+1:]

		if object == "" {
			continue
		}

		if typ == "f" {
			// File

			fkey := fmt.Sprintf("STATS_FILE_%s", date)

			for i := 0; i < 4; i++ {
				rconn.Send("HINCRBY", fkey, object, v)
				fkey = fkey[:strings.LastIndex(fkey, "_")]
			}

			// Increase the total too
			rconn.Send("INCRBY", "STATS_TOTAL", v)
		} else if typ == "m" {
			// Mirror

			mkey := fmt.Sprintf("STATS_MIRROR_%s", date)

			for i := 0; i < 4; i++ {
				rconn.Send("HINCRBY", mkey, object, v)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else if typ == "s" {
			// Bytes

			mkey := fmt.Sprintf("STATS_MIRROR_BYTES_%s", date)

			for i := 0; i < 4; i++ {
				rconn.Send("HINCRBY", mkey, object, v)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else if typ == "p" {
			// Platform

			mkey := fmt.Sprintf("STATS_USERAGENT_platform_%s", date)
			for i := 0; i < 4; i++ {
				rconn.Send("ZINCRBY", mkey, v, object)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else if typ == "o" {
			// OS

			mkey := fmt.Sprintf("STATS_USERAGENT_os_%s", date)
			for i := 0; i < 4; i++ {
				rconn.Send("ZINCRBY", mkey, v, object)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else if typ == "b" {
			// Browser

			mkey := fmt.Sprintf("STATS_USERAGENT_browser_%s", date)
			for i := 0; i < 4; i++ {
				rconn.Send("ZINCRBY", mkey, v, object)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else if typ == "P" {
			// Special Platform

			mkey := fmt.Sprintf("STATS_SPECIAL_platform_%s", date)
			for i := 0; i < 4; i++ {
				rconn.Send("ZINCRBY", mkey, v, object)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else if typ == "O" {
			// Special OS

			mkey := fmt.Sprintf("STATS_SPECIAL_os_%s", date)
			for i := 0; i < 4; i++ {
				rconn.Send("ZINCRBY", mkey, v, object)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else if typ == "B" {
			// Special Browser

			mkey := fmt.Sprintf("STATS_SPECIAL_browser_%s", date)
			for i := 0; i < 4; i++ {
				rconn.Send("ZINCRBY", mkey, v, object)
				mkey = mkey[:strings.LastIndex(mkey, "_")]
			}
		} else {
			log.Warning("Stats: unknown type", typ)
		}
	}

	_, err := rconn.Do("EXEC")

	if err != nil {
		log.Errorf("Stats: could not save stats to redis: %s", err.Error())
		return
	}

	// Clear the map
	s.mapStats = make(map[string]int64)
}
