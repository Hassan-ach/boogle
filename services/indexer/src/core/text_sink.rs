use std::{
    cell::{Cell, RefCell},
    collections::HashMap,
    io::{Cursor, Error},
    rc::Rc,
};

use html5ever::{
    Attribute, QualName,
    interface::{ElementFlags, NodeOrText, QuirksMode, TreeSink},
    local_name, ns, parse_document,
    tendril::{StrTendril, TendrilSink},
};

/// Extracts word frequencies from an HTML document.
///
/// A `TreeSink` is used rather than a string-stripping pass because the tokenizer
/// alone reveals where text is actually separated: stripping tags would join the
/// words either side of a `<div>` into one.
pub fn parse(html: String) -> Result<HashMap<String, u32>, Error> {
    parse_document(TextSink::new(), Default::default())
        .from_utf8()
        .read_from(&mut Cursor::new(html.as_bytes()))
}

#[derive(Debug, Clone)]
enum Node {
    Element(QualName),
}

type Handle = Rc<Node>;

/// A `TreeSink` that keeps only the word counts and discards the tree it is fed.
///
/// `words` is the result. `pending` and `boundary` exist only to rejoin words the
/// tokenizer split; see `process_text`.
#[derive(Debug)]
struct TextSink {
    words: RefCell<HashMap<String, u32>>,
    pending: RefCell<String>,
    boundary: Cell<bool>,
    doc: Handle,
}

impl TextSink {
    fn new() -> Self {
        Self {
            words: RefCell::new(HashMap::new()),
            pending: RefCell::new(String::new()),
            boundary: Cell::new(false),
            doc: Handle::new(Node::Element(QualName::new(None, ns!(), local_name!("")))),
        }
    }

    /// Counts one whitespace-delimited token.
    ///
    /// Punctuation is trimmed first, then the token is dropped unless it is
    /// entirely alphabetic: the index stores words, so numbers and identifiers
    /// such as `utf8` would otherwise become their own low-value entries.
    fn ingest(words: &mut HashMap<String, u32>, raw_word: &str) {
        let word = raw_word.trim_matches(is_word_punctuation);

        if word.is_empty() {
            return;
        }

        if !word.chars().all(char::is_alphabetic) {
            return;
        }

        let word = word.to_lowercase();

        *words.entry(word).or_insert(0) += 1;
    }

    /// Feeds one chunk of character data, keeping any partial trailing word in
    /// `pending`.
    ///
    /// html5ever delivers text in chunks that can fall anywhere, including in the
    /// middle of a word: `<b>ex</b>ample` arrives as two chunks. Without `pending`
    /// that would be indexed as "ex" and "ample", so the trailing partial word is
    /// held back until more text or a boundary confirms it.
    ///
    /// `boundary` records that a non-text node intervened. Without it, text on
    /// either side of a `<div>` would be concatenated into one bogus word.
    fn process_text(&self, text: &str) {
        let mut pending = self.pending.borrow_mut();

        if self.boundary.get() {
            self.boundary.set(false);
            if !pending.is_empty() {
                let carried = std::mem::take(&mut *pending);
                let mut words = self.words.borrow_mut();
                Self::ingest(&mut words, &carried);
            }
        }

        let combined = std::mem::take(&mut *pending) + text;

        let (processable, carry) = if combined.is_empty() || combined.ends_with(char::is_whitespace)
        {
            (combined.as_str(), String::new())
        } else {
            match combined.rfind(char::is_whitespace) {
                Some(i) => (&combined[..i], combined[i..].trim_start().to_string()),
                None => ("", combined.clone()),
            }
        };

        {
            let mut words = self.words.borrow_mut();
            for raw_word in processable.split_whitespace() {
                Self::ingest(&mut words, raw_word);
            }
        }

        *pending = carry;
    }
}

fn is_word_punctuation(c: char) -> bool {
    matches!(
        c,
        '.' | ',' | ':' | '/' | ';' | '"' | '\'' | '!' | '?' | '(' | ')' | '[' | ']'
    )
}

impl TreeSink for TextSink {
    type Output = HashMap<String, u32>;
    type Handle = Handle;
    type ElemName<'a> = &'a QualName;

    fn finish(self) -> Self::Output {
        let mut words = self.words.into_inner();
        let pending = self.pending.into_inner();
        if !pending.is_empty() {
            Self::ingest(&mut words, &pending);
        }
        words
    }

    fn parse_error(&self, _msg: std::borrow::Cow<'static, str>) {}

    fn get_document(&self) -> Self::Handle {
        self.doc.clone()
    }

    fn elem_name<'a>(&'a self, target: &'a Self::Handle) -> Self::ElemName<'a> {
        let Node::Element(name) = target.as_ref();
        name
    }

    fn create_element(&self, name: QualName, _: Vec<Attribute>, _: ElementFlags) -> Self::Handle {
        self.boundary.set(true);
        Handle::new(Node::Element(name))
    }

    fn create_comment(&self, _text: StrTendril) -> Self::Handle {
        Handle::new(Node::Element(QualName::new(None, ns!(), local_name!(""))))
    }

    fn create_pi(&self, _target: StrTendril, _data: StrTendril) -> Self::Handle {
        Handle::new(Node::Element(QualName::new(None, ns!(), local_name!("pi"))))
    }

    fn append(&self, parent: &Self::Handle, child: NodeOrText<Self::Handle>) {
        let NodeOrText::AppendText(text) = child else {
            return;
        };

        let Node::Element(name) = parent.as_ref();
        let local = name.local.as_ref();

        if local == "script" || local == "style" {
            return;
        }

        self.process_text(text.as_ref());
    }

    fn append_based_on_parent_node(
        &self,
        _element: &Self::Handle,
        _prev_element: &Self::Handle,
        _child: NodeOrText<Self::Handle>,
    ) {
    }

    fn append_doctype_to_document(
        &self,
        _name: StrTendril,
        _public_id: StrTendril,
        _system_id: StrTendril,
    ) {
    }

    fn mark_script_already_started(&self, _node: &Self::Handle) {}

    fn pop(&self, _node: &Self::Handle) {}

    fn get_template_contents(&self, _: &Self::Handle) -> Self::Handle {
        Handle::new(Node::Element(QualName::new(
            None,
            ns!(),
            local_name!("template"),
        )))
    }

    fn same_node(&self, a: &Self::Handle, b: &Self::Handle) -> bool {
        Rc::ptr_eq(a, b)
    }

    fn set_quirks_mode(&self, _mode: QuirksMode) {}

    fn append_before_sibling(&self, _sibling: &Self::Handle, _new_node: NodeOrText<Self::Handle>) {}

    fn add_attrs_if_missing(&self, _target: &Self::Handle, _attrs: Vec<Attribute>) {}

    fn associate_with_form(
        &self,
        _target: &Self::Handle,
        _form: &Self::Handle,
        _nodes: (&Self::Handle, Option<&Self::Handle>),
    ) {
    }

    fn remove_from_parent(&self, _target: &Self::Handle) {}

    fn reparent_children(&self, _node: &Self::Handle, _new_parent: &Self::Handle) {}

    fn is_mathml_annotation_xml_integration_point(&self, _handle: &Self::Handle) -> bool {
        false
    }

    fn set_current_line(&self, _line_number: u64) {}

    fn allow_declarative_shadow_roots(&self, _intended_parent: &Self::Handle) -> bool {
        true
    }

    fn attach_declarative_shadow(
        &self,
        _location: &Self::Handle,
        _template: &Self::Handle,
        _attrs: &[Attribute],
    ) -> bool {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn words_of(html: &str) -> HashMap<String, u32> {
        parse(html.to_string()).expect("parse should succeed")
    }

    #[test]
    fn counts_repeated_words() {
        let words = words_of("<body><p>cat dog cat</p></body>");
        assert_eq!(words.get("cat"), Some(&2));
        assert_eq!(words.get("dog"), Some(&1));
        assert_eq!(words.len(), 2);
    }

    #[test]
    fn lowercases_every_token() {
        let words = words_of("<body>Hello HELLO HeLLo</body>");
        assert_eq!(words.get("hello"), Some(&3));
        assert_eq!(words.len(), 1, "case variants must collapse to one key");
    }

    #[test]
    fn splits_on_any_whitespace_and_ignores_runs() {
        let words = words_of("<body>  alpha \n\t beta \r\n\n gamma   alpha </body>");
        assert_eq!(words.get("alpha"), Some(&2));
        assert_eq!(words.get("beta"), Some(&1));
        assert_eq!(words.get("gamma"), Some(&1));
        assert_eq!(words.len(), 3);
    }

    #[test]
    fn skips_script_and_style_content() {
        let words = words_of(
            "<body><p>visible</p>\
             <script>var secret = 'hiddenword'; function hiddenword(){}</script>\
             <style>.hiddenword { color: red; }</style></body>",
        );
        assert_eq!(words.get("visible"), Some(&1));
        assert!(
            !words.contains_key("hiddenword"),
            "script/style text must never reach the index: {:?}",
            words
        );
    }

    #[test]
    fn script_with_src_attribute_contributes_nothing() {
        let words = words_of(r#"<body><p>kept</p><script src="/app.js"></script></body>"#);
        assert_eq!(words.get("kept"), Some(&1));
        assert_eq!(words.len(), 1);
    }

    #[test]
    fn trims_leading_and_trailing_punctuation() {
        let words = words_of("<body>\"quoted,\" (paren) [brack] semi; colon: slash/ done.</body>");
        for token in ["quoted", "paren", "brack", "semi", "colon", "done"] {
            assert_eq!(
                words.get(token),
                Some(&1),
                "token {:?} missing from {:?}",
                token,
                words
            );
        }
    }

    #[test]
    fn drops_tokens_that_are_only_punctuation() {
        let words = words_of("<body>word --- ,,, ... word</body>");
        assert_eq!(words.get("word"), Some(&2));
        assert_eq!(words.len(), 1);
    }

    #[test]
    fn drops_tokens_containing_non_alphabetic_characters() {
        let words = words_of("<body>keep drop1 drop-me keep</body>");
        assert_eq!(words.get("keep"), Some(&2));
        assert!(!words.contains_key("drop1"), "digits must be rejected");
        assert!(
            !words.contains_key("drop-me"),
            "interior hyphen must be rejected"
        );
    }

    #[test]
    fn accepts_non_ascii_letters() {
        let words = words_of("<body>naïve café</body>");
        assert_eq!(words.get("naïve"), Some(&1));
        assert_eq!(words.get("café"), Some(&1));
    }

    #[test]
    fn collects_text_from_nested_elements() {
        let words = words_of(
            "<html><body><div><section><article><p>deep <b>nested</b> text</p></article></section></div></body></html>",
        );
        assert_eq!(words.get("deep"), Some(&1));
        assert_eq!(words.get("nested"), Some(&1));
        assert_eq!(words.get("text"), Some(&1));
        assert_eq!(words.len(), 3);
    }

    #[test]
    fn ignores_html_comments() {
        let words = words_of("<body>kept<!-- ghostword --></body>");
        assert_eq!(words.get("kept"), Some(&1));
        assert!(!words.contains_key("ghostword"));
    }

    #[test]
    fn handles_empty_and_whitespace_only_documents() {
        assert!(words_of("").is_empty());
        assert!(words_of("   \n\t  ").is_empty());
    }

    #[test]
    fn survives_malformed_html() {
        let words = words_of("<body><p>alpha<div><span>beta");
        assert_eq!(words.get("alpha"), Some(&1));
        assert_eq!(words.get("beta"), Some(&1));
    }

    #[test]
    fn counts_are_u32_and_match_input_volume() {
        let body = "word ".repeat(10_000);
        let words = words_of(&format!("<body>{body}</body>"));
        assert_eq!(words.get("word"), Some(&10_000));
        assert_eq!(words.len(), 1, "no token may be fragmented: {:?}", words);
        assert_eq!(
            words.values().sum::<u32>(),
            10_000,
            "no token may be duplicated"
        );
    }

    #[test]
    fn words_spanning_tokenizer_chunks_are_not_fragmented() {
        for n in [1usize, 500, 2_000, 10_000, 20_000] {
            let body = "word ".repeat(n);
            let words = words_of(&format!("<body>{body}</body>"));
            assert_eq!(
                words.get("word"),
                Some(&(n as u32)),
                "n={n} produced {} distinct tokens: {:?}",
                words.len(),
                words
            );
            assert_eq!(words.len(), 1, "n={n} leaked fragments: {:?}", words);
        }
    }

    #[test]
    fn text_runs_in_different_elements_stay_separate() {
        let words = words_of("<body><p>alpha</p><p>beta</p></body>");
        assert_eq!(words.get("alpha"), Some(&1), "got {:?}", words);
        assert_eq!(words.get("beta"), Some(&1), "got {:?}", words);
        assert_eq!(words.len(), 2);
    }

    #[test]
    fn inline_markup_produces_no_garbage_tokens() {
        let source = "abcdef";
        let words = words_of("<body><p>ab<b>cd</b>ef</p></body>");
        assert!(!words.is_empty(), "text must not be dropped entirely");
        for token in words.keys() {
            assert!(
                source.contains(token.as_str()),
                "invented token {:?} not present in source",
                token
            );
        }
        let total: u32 = words.values().sum();
        assert_eq!(
            total, 2,
            "the element boundary must split the runs: {:?}",
            words
        );
        let chars: usize = words.keys().map(|k| k.len()).sum();
        assert_eq!(
            chars,
            source.len(),
            "every character must be accounted for exactly once: {:?}",
            words
        );
    }

    #[test]
    fn words_split_across_chunks_within_one_element_are_rejoined() {
        let long_run = "alpha beta gamma delta ".repeat(2_000);
        let words = words_of(&format!("<body><p>{long_run}</p></body>"));
        assert_eq!(words.get("alpha"), Some(&2_000));
        assert_eq!(words.len(), 4, "no fragmented tokens: {:?}", words);
    }

    #[test]
    fn trailing_word_without_closing_whitespace_is_still_counted() {
        let words = words_of("<body><p>alpha</p><p>beta</p>");
        assert_eq!(words.get("alpha"), Some(&1));
        assert_eq!(
            words.get("beta"),
            Some(&1),
            "a final word must not be dropped"
        );
    }

    #[test]
    fn is_word_punctuation_covers_expected_punctuation() {
        for c in [
            '.', ',', ':', '/', ';', '"', '\'', '!', '?', '(', ')', '[', ']',
        ] {
            assert!(
                is_word_punctuation(c),
                "{c:?} should be treated as punctuation"
            );
        }
        for c in ['a', 'Z', '0', '-', '@', '#', '$'] {
            assert!(
                !is_word_punctuation(c),
                "{c:?} should not be treated as punctuation"
            );
        }
    }
}
