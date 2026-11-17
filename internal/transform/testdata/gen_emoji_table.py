import json, sys
src, out, date = sys.argv[1], sys.argv[2], sys.argv[3]
d = json.load(open(src, encoding="utf-8"))
def esc(s):
    return "".join("\\U%08x" % ord(c) if ord(c) > 0xFFFF else "\\u%04x" % ord(c) for c in s)
rows, tones = {}, set()
for e in d:
    for a in e["aliases"]:
        if a in rows:
            raise SystemExit("duplicate alias " + a)
        rows[a] = e["emoji"]
        if e.get("skin_tones"):
            tones.add(a)
with open(out, "w", encoding="utf-8") as f:
    f.write("// Code generated from the gemoji database; DO NOT EDIT.\n//\n")
    f.write("// Source:    https://raw.githubusercontent.com/github/gemoji/master/db/emoji.json\n")
    f.write("// License:   MIT (Copyright (c) GitHub, Inc.), https://github.com/github/gemoji/blob/master/LICENSE\n")
    f.write("// Generated: %s\n//\n" % date)
    f.write("// To regenerate (there is no go:generate directive on purpose: the build must\n")
    f.write("// not need the network), download emoji.json from the URL above and run\n//\n")
    f.write("//\tpython3 testdata/gen_emoji_table.py emoji.json emoji_table.go YYYY-MM-DD\n")
    f.write("//\tgofmt -w emoji_table.go\n//\n")
    f.write("// Every alias of every entry becomes an emojiTable row; aliases of entries\n")
    f.write("// with \"skin_tones\": true are also listed in emojiToneBase. Slack-specific\n")
    f.write("// names live in emoji.go, not here.\n\n")
    f.write("package transform\n\n")
    f.write("// emojiTable maps a shortcode (without colons) to its Unicode string.\n")
    f.write("// %d shortcodes for %d emoji.\n" % (len(rows), len(d)))
    f.write("var emojiTable = map[string]string{\n")
    for a in sorted(rows):
        f.write('\t%s: "%s",\n' % (json.dumps(a), esc(rows[a])))
    f.write("}\n\n")
    f.write("// emojiToneBase lists the shortcodes whose emoji accepts a skin tone\n// modifier. %d shortcodes.\n" % len(tones))
    f.write("var emojiToneBase = map[string]bool{\n")
    for a in sorted(tones):
        f.write('\t%s: true,\n' % json.dumps(a))
    f.write("}\n")
print(len(rows), len(tones))
