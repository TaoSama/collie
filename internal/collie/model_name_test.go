package collie

import "testing"

// Every place a model shows uses its display name, learnt from the agents'
// model lists; an id no list names shows as it is.
func TestWebModelNames(t *testing.T) {
	runAppJS(t, `
const { modelName, learnModels, aliasPins, followAlias } = loadApp("modelName, learnModels, aliasPins, followAlias");
const eq = (id, want) => assert.equal(modelName(id), want, id);

eq("claude-opus-5-5[1m]", "Opus 5.5");
eq("claude-fable-5-1", "Fable 5.1");
eq("claude-opus-5", "Opus 5");
eq("claude-haiku-4-5-20251001", "Haiku 4.5");
eq("claude-opus-4-20250514", "Opus 4");
eq("fable", "Fable"); // no list says which Fable yet
eq("gpt-6.2-sol", "gpt-6.2-sol");
eq("openrouter/some-model", "openrouter/some-model");
eq("", "");

learnModels({ models: [
  { id: "fable", label: "Fable" },
  { id: "gpt-6.2-sol", label: "GPT-6.2 Sol" },
  { id: "claude-fable-5-1", label: "Claude Fable 5.1" },
  { id: "claude-fable-5" },
  { id: "claude-opus-4-6", label: "Claude Opus 4.6" },
  { id: "claude-opus-5-5", label: "Claude Opus 5.5" },
  { id: "gemini-3.8-flash-high", label: "Gemini 3.8 Flash High" },
]});
learnModels({ models: [{ id: "auto", label: "Auto (best for the task)" }, { id: "gemini-4-neon", label: "Gemini 4 Neon" }, { id: "gemini-4-hydrogen", label: "Gemini 4 Hydrogen" }] });

eq("fable", "Fable 5.1");          // a Claude alias: its family's newest
eq("fable[1m]", "Fable 5.1");
eq("opus", "Opus 5.5");
eq("sonnet", "Sonnet");            // no Sonnet listed anywhere
eq("gpt-6.2-sol", "GPT-6.2 Sol");
eq("gemini-3.8-flash-high[1m]", "Gemini 3.8 Flash High");
eq("gemini-4-neon", "Gemini 4 Neon");
eq("gemini-4-hydrogen", "Gemini 4 Hydrogen");
eq("claude-fable-5-2[1m]", "Fable 5.2"); // newer than any list
eq("auto", "Auto (best for the task)"); // another agent's lowercase id keeps its label
eq("seed-unknown[1m]", "seed-unknown[1m]");

assert.equal(JSON.stringify(aliasPins({ model: "fable[1m]", effort: "high" })), JSON.stringify(["Fable 5.1", "高"]));
assert.equal(JSON.stringify(aliasPins({ model: "", effort: "" })), JSON.stringify(["", ""]));
assert.equal(JSON.stringify(aliasPins({ model: "", runs: "claude-opus-5-5[1m]", effort: "" })), JSON.stringify(["Opus 5.5", ""]));

// An alias's model picker shows the model it runs and can't be changed.
const sel = () => ({ disabled: false, title: "", value: "x" });
const shown = (s) => /data-short="([^"]*)"/.exec(s.innerHTML)[1];
let s = sel();
assert.equal(followAlias(s, { name: "ccx", model: "sonnet", runs: "claude-opus-5-5[1m]" }), true);
assert.equal(shown(s), "Opus 5.5");
assert.ok(s.disabled && s.value === "" && !s.innerHTML.includes("跟随"), s.innerHTML);
s = sel();
followAlias(s, { name: "ccf", model: "fable[1m]" }); // an older computer: what it pins
assert.equal(shown(s), "Fable 5.1");
s = sel();
followAlias(s, { name: "ag" }, { default: "gemini-4-neon" }); // nothing known: the agent's default
assert.equal(shown(s), "Gemini 4 Neon");
s = sel();
followAlias(s, { name: "gone" }, {});
assert.equal(shown(s), "默认模型");
s = sel();
assert.equal(followAlias(s, null), false);
assert.equal(s.disabled, false);
console.log("MODEL_NAMES_OK");
`, "MODEL_NAMES_OK")
}
