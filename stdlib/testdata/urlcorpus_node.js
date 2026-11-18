// Records node's answers to the url corpus in url_node.json:
//
//	node stdlib/testdata/urlcorpus_node.js
//
// The working directory each flavor resolves against is set by the corpus,
// and the environment's per-drive working directories are left out, so the
// answers do not depend on where node runs.
const fs = require("fs");
const path = require("path");
const url = require("url");
for (const k of Object.keys(process.env)) {
  if (k.startsWith("=")) delete process.env[k];
}
const run = eval(fs.readFileSync(path.join(__dirname, "urlcorpus.js"), "utf8"));
const out = run(url, (cwd) => { process.cwd = () => cwd; });
fs.writeFileSync(path.join(__dirname, "url_node.json"), JSON.stringify(out, null, 1) + "\n");
console.log(Object.keys(out).length + " answers, node " + process.version);
