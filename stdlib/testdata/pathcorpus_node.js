// Records node's answers to the path corpus in path_node.json:
//
//	node stdlib/testdata/pathcorpus_node.js
//
// The working directory each flavor resolves against is set by the corpus,
// and the environment's per-drive working directories are left out, so the
// answers do not depend on where node runs.
const fs = require("fs");
const path = require("path");
for (const k of Object.keys(process.env)) {
  if (k.startsWith("=")) delete process.env[k];
}
const run = eval(fs.readFileSync(path.join(__dirname, "pathcorpus.js"), "utf8"));
const out = run(path, (cwd) => { process.cwd = () => cwd; });
fs.writeFileSync(path.join(__dirname, "path_node.json"), JSON.stringify(out, null, 1) + "\n");
console.log(Object.keys(out).length + " answers, node " + process.version);
