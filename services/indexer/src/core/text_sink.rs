use std::{
    cell::RefCell,
    collections::HashMap,
    io::{Cursor, Error},
    rc::Rc,
};

use html5ever::{
    interface::{ElementFlags, NodeOrText, QuirksMode, TreeSink},
    local_name, ns, parse_document,
    tendril::{StrTendril, TendrilSink},
    Attribute, QualName,
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

#[derive(Debug)]
struct TextSink {
    words: RefCell<HashMap<String, u32>>,
    doc: Handle,
}

impl TextSink {
    fn new() -> Self {
        Self {
            words: RefCell::new(HashMap::new()),
            doc: Handle::new(Node::Element(QualName::new(None, ns!(), local_name!("")))),
        }
    }

    fn process_text(&self, text: &str) {
        let mut words = self.words.borrow_mut();

        for raw_word in text.split_whitespace() {
            let word = raw_word.trim_matches(is_word_punctuation);

            if word.is_empty() {
                continue;
            }

            if !word.chars().all(char::is_alphabetic) {
                continue;
            }

            let word = word.to_lowercase();

            *words.entry(word).or_insert(0) += 1;
        }
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
        self.words.into_inner()
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
