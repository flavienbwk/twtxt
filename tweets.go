// -*- tab-width: 4; -*-

package twtxt

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	feedsDir = "feeds"
)

type Tweeter struct {
	Nick string
	URL  string
}

type Tweet struct {
	Tweeter Tweeter
	Text    string
	Created time.Time
}

// typedef to be able to attach sort methods
type Tweets []Tweet

func (tweets Tweets) Len() int {
	return len(tweets)
}
func (tweets Tweets) Less(i, j int) bool {
	return tweets[i].Created.Before(tweets[j].Created)
}
func (tweets Tweets) Swap(i, j int) {
	tweets[i], tweets[j] = tweets[j], tweets[i]
}

func (tweets Tweets) Tags() map[string]int {
	tags := make(map[string]int)
	re := regexp.MustCompile(`#[-\w]+`)
	for _, tweet := range tweets {
		for _, tag := range re.FindAllString(tweet.Text, -1) {
			tags[strings.TrimLeft(tag, "#")]++
		}
	}
	return tags
}

// Turns "@nick" into "@<nick URL>" if we're following nick.
func ExpandMentions(text string, user *User) string {
	re := regexp.MustCompile(`@([_a-zA-Z0-9]+)`)
	return re.ReplaceAllStringFunc(text, func(match string) string {
		parts := re.FindStringSubmatch(match)
		mentionednick := parts[1]

		for followednick, followedurl := range user.Following {
			if mentionednick == followednick {
				return fmt.Sprintf("@<%s %s>", followednick, followedurl)
			}
		}
		// Not expanding if we're not following
		return match
	})
}

func AppendSpecial(path, specialUser, text string) error {
	user := &User{Username: specialUser}
	user.Following = make(map[string]string)
	return AppendTweet(path, text, user)
}

func AppendTweet(path, text string, user *User) error {
	p := filepath.Join(path, feedsDir)
	if err := os.MkdirAll(p, 0755); err != nil {
		log.WithError(err).Error("error creating feeds directory")
		return err
	}

	fn := filepath.Join(p, user.Username)
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("cowardly refusing to tweet empty text, or only spaces")
	}

	text = fmt.Sprintf("%s\t%s\n", time.Now().Format(time.RFC3339), ExpandMentions(text, user))
	f, err := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err = f.WriteString(text); err != nil {
		return err
	}

	return nil
}

func GetAllTweets(conf *Config) (Tweets, error) {
	p := filepath.Join(conf.Data, feedsDir)
	if err := os.MkdirAll(p, 0755); err != nil {
		log.WithError(err).Error("error creating feeds directory")
		return nil, err
	}

	files, err := ioutil.ReadDir(p)
	if err != nil {
		log.WithError(err).Error("error listing feeds")
		return nil, err
	}

	var tweets Tweets

	for _, info := range files {
		tweeter := Tweeter{
			Nick: info.Name(),
			URL:  fmt.Sprintf("%s/u/%s", strings.TrimSuffix(conf.BaseURL, "/"), info.Name()),
		}
		fn := filepath.Join(p, info.Name())
		f, err := os.Open(fn)
		if err != nil {
			log.WithError(err).Warnf("error opening feed: %s", fn)
			continue
		}
		s := bufio.NewScanner(f)
		t, err := ParseFile(s, tweeter)
		if err != nil {
			log.WithError(err).Errorf("error processing feed %s", fn)
			continue
		}
		tweets = append(tweets, t...)
		f.Close()
	}

	return tweets, nil
}

func ParseFile(scanner *bufio.Scanner, tweeter Tweeter) (Tweets, error) {
	var tweets Tweets
	re := regexp.MustCompile(`^(.+?)(\s+)(.+)$`) // .+? is ungreedy
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts := re.FindStringSubmatch(line)
		// "Submatch 0 is the match of the entire expression, submatch 1 the
		// match of the first parenthesized subexpression, and so on."
		if len(parts) != 4 {
			log.Warnf("could not parse: '%s' (source:%s)\n", line, tweeter.URL)
			continue
		}
		tweets = append(tweets,
			Tweet{
				Tweeter: tweeter,
				Created: ParseTime(parts[1]),
				Text:    parts[3],
			})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return tweets, nil
}

func ParseTime(timestr string) time.Time {
	var tm time.Time
	var err error
	// Twtxt clients generally uses basically time.RFC3339Nano, but sometimes
	// there's a colon in the timezone, or no timezone at all.
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999Z0700",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04.999999999Z07:00",
		"2006-01-02T15:04.999999999Z0700",
		"2006-01-02T15:04.999999999",
	} {
		tm, err = time.Parse(layout, strings.ToUpper(timestr))
		if err != nil {
			continue
		} else {
			break
		}
	}
	if err != nil {
		return time.Unix(0, 0)
	}
	return tm
}
