package bot

import (
	"fmt"
	"regexp"
	"strings"
)

// escapeHTML escapes HTML special characters for safe embedding
func escapeHTML(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	return text
}

// toHTML converts Claude's markdown output to Telegram HTML format
func toHTML(text string) string {
	// Step 1: Extract and preserve code blocks
	codeBlockPattern := regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")
	codeBlocks := []string{}
	text = codeBlockPattern.ReplaceAllStringFunc(text, func(match string) string {
		submatch := codeBlockPattern.FindStringSubmatch(match)
		lang := submatch[1]
		code := submatch[2]

		// Escape HTML in code
		escapedCode := escapeHTML(code)

		var htmlBlock string
		if lang != "" {
			htmlBlock = fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>", lang, escapedCode)
		} else {
			htmlBlock = fmt.Sprintf("<pre><code>%s</code></pre>", escapedCode)
		}

		placeholder := fmt.Sprintf("\x00CB%d\x00", len(codeBlocks))
		codeBlocks = append(codeBlocks, htmlBlock)
		return placeholder
	})

	// Step 2: Extract and preserve inline code
	inlineCodePattern := regexp.MustCompile("`([^`]+)`")
	inlineCodes := []string{}
	text = inlineCodePattern.ReplaceAllStringFunc(text, func(match string) string {
		submatch := inlineCodePattern.FindStringSubmatch(match)
		code := submatch[1]

		// Escape HTML in code
		escapedCode := escapeHTML(code)
		htmlCode := fmt.Sprintf("<code>%s</code>", escapedCode)

		placeholder := fmt.Sprintf("\x00IC%d\x00", len(inlineCodes))
		inlineCodes = append(inlineCodes, htmlCode)
		return placeholder
	})

	// Step 3: Escape HTML special chars in remaining text
	text = escapeHTML(text)

	// Step 4: Convert markdown formatting
	// Bold: **text** -> <b>text</b>
	boldPattern := regexp.MustCompile(`\*\*([^\*]+)\*\*`)
	text = boldPattern.ReplaceAllString(text, "<b>$1</b>")

	// Italic: *text* -> <i>text</i>
	italicPattern1 := regexp.MustCompile(`\*([^\*]+)\*`)
	text = italicPattern1.ReplaceAllString(text, "<i>$1</i>")

	// Strikethrough: ~~text~~ -> <s>text</s>
	strikePattern := regexp.MustCompile(`~~([^~]+)~~`)
	text = strikePattern.ReplaceAllString(text, "<s>$1</s>")

	// Step 5: Restore inline code
	for i, htmlCode := range inlineCodes {
		placeholder := fmt.Sprintf("\x00IC%d\x00", i)
		text = strings.ReplaceAll(text, placeholder, htmlCode)
	}

	// Step 6: Restore code blocks
	for i, htmlBlock := range codeBlocks {
		placeholder := fmt.Sprintf("\x00CB%d\x00", i)
		text = strings.ReplaceAll(text, placeholder, htmlBlock)
	}

	return text
}
