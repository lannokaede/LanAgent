package interview

import _ "embed"

//go:embed skills/go-interview/SKILL.md
var SkillBody string

const SkillDescription = "Go 后端模拟面试、答题点评、暂停讲解或查资料后继续、薄弱点复习。"
