// Copyright 2023 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scheduling

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/tikv/pd/pkg/keyspace"
	"github.com/tikv/pd/pkg/mcs/scheduling/server/rule"
	"github.com/tikv/pd/pkg/mcs/utils"
	"github.com/tikv/pd/pkg/schedule/labeler"
	"github.com/tikv/pd/pkg/schedule/placement"
	"github.com/tikv/pd/pkg/storage/endpoint"
	"github.com/tikv/pd/pkg/utils/testutil"
	"github.com/tikv/pd/tests"
)

type ruleTestSuite struct {
	suite.Suite

	ctx    context.Context
	cancel context.CancelFunc

	// The PD cluster.
	cluster *tests.TestCluster
	// pdLeaderServer is the leader server of the PD cluster.
	pdLeaderServer *tests.TestServer
}

func TestRule(t *testing.T) {
	suite.Run(t, &ruleTestSuite{})
}

func (suite *ruleTestSuite) SetupSuite() {
	re := suite.Require()

	var err error
	suite.ctx, suite.cancel = context.WithCancel(context.Background())
	suite.cluster, err = tests.NewTestAPICluster(suite.ctx, 1)
	re.NoError(err)
	err = suite.cluster.RunInitialServers()
	re.NoError(err)
	leaderName := suite.cluster.WaitLeader()
	suite.pdLeaderServer = suite.cluster.GetServer(leaderName)
	re.NoError(suite.pdLeaderServer.BootstrapCluster())
}

func (suite *ruleTestSuite) TearDownSuite() {
	suite.cancel()
	suite.cluster.Destroy()
}

func loadRules(re *require.Assertions, ruleStorage endpoint.RuleStorage) (rules []*placement.Rule) {
	err := ruleStorage.LoadRules(func(_, v string) {
		r, err := placement.NewRuleFromJSON([]byte(v))
		re.NoError(err)
		rules = append(rules, r)
	})
	re.NoError(err)
	return
}

func loadRuleGroups(re *require.Assertions, ruleStorage endpoint.RuleStorage) (groups []*placement.RuleGroup) {
	err := ruleStorage.LoadRuleGroups(func(_, v string) {
		rg, err := placement.NewRuleGroupFromJSON([]byte(v))
		re.NoError(err)
		groups = append(groups, rg)
	})
	re.NoError(err)
	return
}

func loadRegionRules(re *require.Assertions, ruleStorage endpoint.RuleStorage) (rules []*labeler.LabelRule) {
	err := ruleStorage.LoadRegionRules(func(_, v string) {
		lr, err := labeler.NewLabelRuleFromJSON([]byte(v))
		re.NoError(err)
		rules = append(rules, lr)
	})
	re.NoError(err)
	return
}

func (suite *ruleTestSuite) TestRuleWatch() {
	re := suite.Require()

	// Create a rule watcher.
	watcher, err := rule.NewWatcher(
		suite.ctx,
		suite.pdLeaderServer.GetEtcdClient(),
		suite.cluster.GetCluster().GetId(),
	)
	re.NoError(err)
	ruleStorage := watcher.GetRuleStorage()
	// Check the default rule.
	rules := loadRules(re, ruleStorage)
	re.Len(rules, 1)
	re.Equal("pd", rules[0].GroupID)
	re.Equal("default", rules[0].ID)
	re.Equal(0, rules[0].Index)
	re.Empty(rules[0].StartKey)
	re.Empty(rules[0].EndKey)
	re.Equal(placement.Voter, rules[0].Role)
	re.Empty(rules[0].LocationLabels)
	// Check the empty rule group.
	ruleGroups := loadRuleGroups(re, ruleStorage)
	re.NoError(err)
	re.Empty(ruleGroups)
	// Set a new rule via the PD API server.
	ruleManager := suite.pdLeaderServer.GetRaftCluster().GetRuleManager()
	rule := &placement.Rule{
		GroupID:     "2",
		ID:          "3",
		Role:        "voter",
		Count:       1,
		StartKeyHex: "22",
		EndKeyHex:   "dd",
	}
	err = ruleManager.SetRule(rule)
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		rules = loadRules(re, ruleStorage)
		return len(rules) == 2
	})
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].ID > rules[j].ID
	})
	re.Len(rules, 2)
	re.Equal(rule.GroupID, rules[1].GroupID)
	re.Equal(rule.ID, rules[1].ID)
	re.Equal(rule.Role, rules[1].Role)
	re.Equal(rule.Count, rules[1].Count)
	re.Equal(rule.StartKeyHex, rules[1].StartKeyHex)
	re.Equal(rule.EndKeyHex, rules[1].EndKeyHex)
	// Delete the rule.
	err = ruleManager.DeleteRule(rule.GroupID, rule.ID)
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		rules = loadRules(re, ruleStorage)
		return len(rules) == 1
	})
	re.Len(rules, 1)
	re.Equal("pd", rules[0].GroupID)
	// Create a new rule group.
	ruleGroup := &placement.RuleGroup{
		ID:       "2",
		Index:    100,
		Override: true,
	}
	err = ruleManager.SetRuleGroup(ruleGroup)
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		ruleGroups = loadRuleGroups(re, ruleStorage)
		return len(ruleGroups) == 1
	})
	re.Len(ruleGroups, 1)
	re.Equal(ruleGroup.ID, ruleGroups[0].ID)
	re.Equal(ruleGroup.Index, ruleGroups[0].Index)
	re.Equal(ruleGroup.Override, ruleGroups[0].Override)
	// Delete the rule group.
	err = ruleManager.DeleteRuleGroup(ruleGroup.ID)
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		ruleGroups = loadRuleGroups(re, ruleStorage)
		return len(ruleGroups) == 0
	})
	re.Empty(ruleGroups)

	// Test the region label rule watch.
	labelRules := loadRegionRules(re, ruleStorage)
	re.Len(labelRules, 1)
	defaultKeyspaceRule := keyspace.MakeLabelRule(utils.DefaultKeyspaceID)
	re.Equal(defaultKeyspaceRule, labelRules[0])
	// Set a new region label rule.
	labelRule := &labeler.LabelRule{
		ID:       "rule1",
		Labels:   []labeler.RegionLabel{{Key: "k1", Value: "v1"}},
		RuleType: "key-range",
		Data:     labeler.MakeKeyRanges("1234", "5678"),
	}
	regionLabeler := suite.pdLeaderServer.GetRaftCluster().GetRegionLabeler()
	err = regionLabeler.SetLabelRule(labelRule)
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		labelRules = loadRegionRules(re, ruleStorage)
		return len(labelRules) == 2
	})
	sort.Slice(labelRules, func(i, j int) bool {
		return labelRules[i].ID < labelRules[j].ID
	})
	re.Len(labelRules, 2)
	re.Equal(labelRule.ID, labelRules[1].ID)
	re.Equal(labelRule.Labels, labelRules[1].Labels)
	re.Equal(labelRule.RuleType, labelRules[1].RuleType)
	// Patch the region label rule.
	labelRule = &labeler.LabelRule{
		ID:       "rule2",
		Labels:   []labeler.RegionLabel{{Key: "k2", Value: "v2"}},
		RuleType: "key-range",
		Data:     labeler.MakeKeyRanges("ab12", "cd12"),
	}
	patch := labeler.LabelRulePatch{
		SetRules:    []*labeler.LabelRule{labelRule},
		DeleteRules: []string{"rule1"},
	}
	err = regionLabeler.Patch(patch)
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		labelRules = loadRegionRules(re, ruleStorage)
		return len(labelRules) == 2
	})
	sort.Slice(labelRules, func(i, j int) bool {
		return labelRules[i].ID < labelRules[j].ID
	})
	re.Len(labelRules, 2)
	re.Equal(defaultKeyspaceRule, labelRules[0])
	re.Equal(labelRule.ID, labelRules[1].ID)
	re.Equal(labelRule.Labels, labelRules[1].Labels)
	re.Equal(labelRule.RuleType, labelRules[1].RuleType)
}
