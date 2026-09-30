use std::{borrow::Cow, cell::RefCell, collections::HashMap, io::Cursor, io::Error, rc::Rc};

use html5ever::{
    Attribute, QualName,
    interface::{ElementFlags, NodeOrText, QuirksMode, TreeSink},
    local_name, ns, parse_document,
    tendril::{StrTendril, TendrilSink},
};

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

#[derive(Debug, Clone)]
struct TextSink {
    pub texts: RefCell<Vec<StrTendril>>,
    pub doc: Handle,
}

impl TextSink {
    pub fn new() -> Self {
        Self {
            texts: RefCell::new(Vec::new()),
            doc: Handle::new(Node::Element(QualName::new(None, ns!(), local_name!("")))),
        }
    }
}

impl TreeSink for TextSink {
    type Output = HashMap<String, u32>;
    type Handle = Handle;
    type ElemName<'a> = &'a QualName;

    fn finish(self) -> Self::Output {
        let mut out: HashMap<String, u32> = HashMap::new();
        for text in self.texts.into_inner() {
            for word in text.to_lowercase().split_whitespace().filter_map(|w| {
                let trimed_word = w.trim_matches(|c: char| {
                    c.is_whitespace()
                        || matches!(
                            c,
                            '.' | ','
                                | ':'
                                | '/'
                                | ';'
                                | '"'
                                | '\''
                                | '!'
                                | '?'
                                | '('
                                | ')'
                                | '['
                                | ']'
                        )
                });
                if trimed_word.is_empty() || trimed_word.parse::<u32>().is_ok() {
                    return None;
                }
                // remove every word that is not an English word
                // so no symbols in my data words
                for c in trimed_word.chars() {
                    if !c.is_alphabetic() {
                        return None;
                    }
                }
                Some(trimed_word.to_string())
            }) {
                *out.entry(word).or_insert(0) += 1;
            }
        }
        out
    }

    fn parse_error(&self, _msg: Cow<'static, str>) {
        // eprintln!("Parse error: {_msg}");
    }

    fn get_document(&self) -> Self::Handle {
        self.doc.clone()
    }

    fn elem_name<'a>(&'a self, target: &'a Self::Handle) -> Self::ElemName<'a> {
        let Node::Element(name) = target.as_ref();
        name
    }

    fn create_comment(&self, _text: StrTendril) -> Self::Handle {
        Handle::new(Node::Element(QualName::new(None, ns!(), local_name!(""))))
    }

    fn create_element(&self, name: QualName, _: Vec<Attribute>, _: ElementFlags) -> Self::Handle {
        Handle::new(Node::Element(name))
    }

    fn create_pi(&self, _target: StrTendril, _data: StrTendril) -> Self::Handle {
        Handle::new(Node::Element(QualName::new(None, ns!(), local_name!("pi"))))
    }

    fn append(&self, parent: &Self::Handle, child: NodeOrText<Self::Handle>) {
        if let NodeOrText::AppendText(t) = child {
            let Node::Element(name) = parent.as_ref();
            let local = name.local.as_ref();
            if local == "script" || local == "style" {
                return; //ignore script and style elements
            }
            self.texts.borrow_mut().push(t);
        }
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
        Handle::new(Node::Element(html5ever::QualName::new(
            None,
            ns!(),
            local_name!("template"),
        )))
    }

    fn same_node(&self, _: &Self::Handle, _: &Self::Handle) -> bool {
        false
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

    #[tokio::test]
    async fn test_parse_html_word_count() {
        let html = r#"
            <!DOCTYPE html>
            <html>
            <head>
                <style>body { color: red; }</style>
                <script>console.log("ignore me");</script>
            </head>
            <body>
                <h1>Search Engine Indexer</h1>
                <p>Boogle is a search engine. Search engine indexing is fast!</p>
            </body>
            </html>
        "#;

        let words = parse(html.to_string()).unwrap();

        assert_eq!(words.get("search"), Some(&3));
        assert_eq!(words.get("engine"), Some(&3));
        assert_eq!(words.get("boogle"), Some(&1));
        assert_eq!(words.get("indexer"), Some(&1));
        // script and style content should be ignored
        assert_eq!(words.get("color"), None);
        assert_eq!(words.get("console"), None);
    }

    #[tokio::test]
    async fn test_parse_html_filters_numbers_and_symbols() {
        let html = r#"
            <!DOCTYPE html>
            <html>
            <head><title>Test</title></head>
            <body>
                <p>Version 1234 test!</p>
            </body>
            </html>
        "#;
        let words = parse(html.to_string()).unwrap();

        assert_eq!(words.get("version"), Some(&1));
        assert_eq!(words.get("test"), Some(&2)); // 1 from title, 1 from p
        assert_eq!(words.get("1234"), None);
    }
}
