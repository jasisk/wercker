//   Copyright 2016 Wercker Holding BV
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package util

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

type UtilSuite struct {
	*TestSuite
}

func TestUtilSuite(t *testing.T) {
	suiteTester := &UtilSuite{&TestSuite{}}
	suite.Run(t, suiteTester)
}

func (s *UtilSuite) TestCounterIncrement() {
	counter := &Counter{}
	s.Equal(0, counter.Current, "expected counter to initialize with 0")

	n1 := counter.Increment()
	s.Equal(0, n1, "expected first increment to be 0")

	n2 := counter.Increment()
	s.Equal(1, n2, "expected second increment to be 0")
}

func (s *UtilSuite) TestCounterIncrement2() {
	counter := &Counter{Current: 3}
	s.Equal(3, counter.Current, "expected counter to initialize with 3")

	n1 := counter.Increment()
	s.Equal(3, n1, "expected first increment to be 3")

	n2 := counter.Increment()
	s.Equal(4, n2, "expected second increment to be 4")
}

func (s *UtilSuite) TestMinInt() {
	testSteps := []struct {
		input    []int
		expected int
	}{
		{[]int{}, 0},
		{[]int{1}, 1},
		{[]int{1, 2}, 1},
		{[]int{2, 1}, 1},
		{[]int{1, 1}, 1},
		{[]int{5, 4, 3, 5, 7, 4}, 3},
	}

	for _, test := range testSteps {
		actual := MinInt(test.input...)

		s.Equal(test.expected, actual)
	}
}

func (s *UtilSuite) TestMaxInt() {
	testSteps := []struct {
		input    []int
		expected int
	}{
		{[]int{}, 0},
		{[]int{1}, 1},
		{[]int{1, 2}, 2},
		{[]int{2, 1}, 2},
		{[]int{1, 1}, 1},
		{[]int{5, 4, 3, 5, 7, 4}, 7},
	}

	for _, test := range testSteps {
		actual := MaxInt(test.input...)

		s.Equal(test.expected, actual)
	}
}

func (s *UtilSuite) TestSplitFunc() {
	testCases := []struct {
		input  string
		output []string
	}{
		{"hello, world", []string{"hello", "world"}},
		{"hello world", []string{"hello", "world"}},
		{"hello,              world", []string{"hello", "world"}},
		{"hello,world", []string{"hello", "world"}},
		{"hello                     world", []string{"hello", "world"}},
	}

	for _, test := range testCases {
		actual := SplitSpaceOrComma(test.input)
		s.Equal(test.output, actual)
		s.Equal(len(test.output), 2)
	}
}

// TestFileInfo is a dummy os.FileInfo for testing
type TestFileInfo struct {
	modTime time.Time
	name    string
}

func (t TestFileInfo) ModTime() time.Time {
	return t.modTime
}

func (t TestFileInfo) Name() string {
	return t.name
}

func (t TestFileInfo) Size() int64 {
	return 0
}

func (t TestFileInfo) Mode() os.FileMode {
	return 0
}

func (t TestFileInfo) IsDir() bool {
	return true
}

func (t TestFileInfo) Sys() interface{} {
	return nil
}

func (s *UtilSuite) TestSortByModDate() {
	// create 5 fake file infos, the first one being the oldest
	dirs := []os.FileInfo{}
	for day := 1; day <= 5; day++ {
		dirs = append(dirs, TestFileInfo{
			// offset modified time so it's jan 1st, 2nd, etc
			modTime: time.Date(2016, 1, day, 12, 0, 0, 0, time.UTC),
			name:    fmt.Sprintf("jan-%v", day),
		})
	}

	// before sort the first item is the one we added first,
	// ignoring the modtime
	s.Equal("jan-1", dirs[0].Name())

	SortByModDate(dirs)

	// after sort the one with the most recent mod time is first
	s.Equal("jan-5", dirs[0].Name())
	s.Equal("jan-4", dirs[1].Name())
	s.Equal("jan-3", dirs[2].Name())
	s.Equal("jan-2", dirs[3].Name())
	s.Equal("jan-1", dirs[4].Name())
}

func (s *UtilSuite) TestConvertUnit() {
	tests := []struct {
		input         int64
		expectedValue int64
		expectedUnit  string
	}{
		{1, 1, "B"},
		{1024, 1, "KiB"},
		{2048, 2, "KiB"},
		{1047552, 1023, "KiB"},
		{1048576, 1, "MiB"},
		{1073741824, 1, "GiB"},
		{1099511627776, 1024, "GiB"},
		{1100585369600, 1025, "GiB"}, // GiB is the last unit
	}

	for _, test := range tests {
		actualValue, actualUnit := ConvertUnit(test.input)

		s.Equal(test.expectedValue, actualValue)
		s.Equal(test.expectedUnit, actualUnit)

	}
}
