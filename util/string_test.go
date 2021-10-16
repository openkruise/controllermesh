/*
Copyright 2021 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import "testing"

func TestLastReplace(t *testing.T) {
	cases := []struct {
		s        string
		old      string
		new      string
		expected string
	}{
		{
			s:        "foo/bar",
			old:      "foo",
			new:      "baz",
			expected: "baz/bar",
		},
		{
			s:        "/api/v1/namespaces/foo/configmaps/bar",
			old:      "bar",
			new:      "baz",
			expected: "/api/v1/namespaces/foo/configmaps/baz",
		},
		{
			s:        "/api/v1/namespaces/foo/configmaps/bar",
			old:      "qaz",
			new:      "baz",
			expected: "/api/v1/namespaces/foo/configmaps/bar",
		},
		{
			s:        "/api/v1/namespaces/foo/configmaps/foo",
			old:      "foo",
			new:      "foo--test",
			expected: "/api/v1/namespaces/foo/configmaps/foo--test",
		},
	}

	for i, tc := range cases {
		got := LastReplace(tc.s, tc.old, tc.new)
		if got != tc.expected {
			t.Fatalf("#%d failed, got %v expected %v", i, got, tc.expected)
		}
	}
}
