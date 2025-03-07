// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package distance

import "testing"

func TestPositivelyRelated(t *testing.T) {
	cases := []struct {
		metricType string
		wanted     bool
	}{
		{
			IP,
			true,
		},
		{
			JACCARD,
			false,
		},
		{
			TANIMOTO,
			false,
		},
		{
			L2,
			false,
		},
		{
			HAMMING,
			false,
		},
		{
			SUPERSTRUCTURE,
			false,
		},
		{
			SUBSTRUCTURE,
			false,
		},
	}

	for idx := range cases {
		if got := PositivelyRelated(cases[idx].metricType); got != cases[idx].wanted {
			t.Errorf("PositivelyRelated(%v) = %v", cases[idx].metricType, cases[idx].wanted)
		}
	}
}
