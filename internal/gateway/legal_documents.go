package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/plan"
)

// The text of the two documents a visitor reads before they sign up: the
// terms of service and the privacy notice. One copy, here: the gateway serves
// them as finished HTML at /terms and /privacy (legal_pages.go), so a reader
// without JavaScript — a search engine, an AI assistant, a screen reader in a
// locked-down browser — gets the text itself, and the console's own page
// (web/src/public/legal/LegalPage.tsx) renders the same document from
// GET /api/legal/documents/{doc} where a proxy sends those paths to it.
//
// They live in the repository rather than in a database because they describe
// what *this software* does with data — which data it holds, for how long,
// who it is sent to — and that is the same wherever the engine runs. What
// differs per deployment is who is accountable for it, so every such value
// comes from LegalConfig (legal.go) and a clause whose value is not
// configured is left out rather than filled with a guess.
//
// Two rules when editing:
//
//   - Only state what the code does. Every factual claim here is verifiable
//     in the repository: the sign-up plan's name, allowance and read-only
//     ending come from the plan the server offers, the backup window from
//     deploy/docker/pg-backup/pg-backup.sh, "no analytics" from web/index.html
//     having no third-party script, the export rights from the four export
//     endpoints. A sentence nobody can check is a liability, not a policy.
//   - Move LEGAL_UPDATED when the substance changes, so a reader can tell
//     which text they agreed to.

// legalBlock is a paragraph, or a list of points rendered as bullets. In JSON
// it is a string or an array of strings, which is what the console renders.
type legalBlock struct {
	Text  string
	Items []string
}

func (b legalBlock) MarshalJSON() ([]byte, error) {
	if b.Items != nil {
		return json.Marshal(b.Items)
	}
	return json.Marshal(b.Text)
}

func para(s string) legalBlock       { return legalBlock{Text: s} }
func points(s ...string) legalBlock  { return legalBlock{Items: s} }
func paras(s ...string) []legalBlock { return mapBlocks(s, para) }
func mapBlocks(s []string, f func(string) legalBlock) []legalBlock {
	out := make([]legalBlock, 0, len(s))
	for _, x := range s {
		out = append(out, f(x))
	}
	return out
}

type legalSection struct {
	Heading string       `json:"heading"`
	Body    []legalBlock `json:"body"`
}

type legalDocument struct {
	Title string `json:"title"`
	// Intro is the line under the title: what this document is.
	Intro    string         `json:"intro"`
	Sections []legalSection `json:"sections"`
}

// legalPlanFacts is what the deployment's self-service edition offers, when
// sign-up is open: bounded by its allowance, never by time.
type legalPlanFacts struct {
	Name      string
	Allowance string
	// Next is the edition's own "where to go from here", if it has one.
	Next string
	// Dormancy is when a Playground workspace nobody uses is retired; nil
	// anywhere else.
	Dormancy *legalDormancy
}

type legalDormancy struct{ WarnDays, DeleteDays, BackupDays int }

// planAllowanceProse is a plan's limits inside a sentence: "5 users,
// 2 applications and 3 models", or "" when it limits nothing worth saying.
// The /signup strip formats the same limits (web/src/public/planAllowance.ts);
// zero means unlimited everywhere in internal/plan, so a zero is left out.
func planAllowanceProse(l plan.Limits) string {
	plural := func(n int, word string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, word)
		}
		return fmt.Sprintf("%d %ss", n, word)
	}
	var parts []string
	if l.MaxUsers > 0 {
		parts = append(parts, plural(l.MaxUsers, "user"))
	}
	if l.MaxApplications > 0 {
		parts = append(parts, plural(l.MaxApplications, "application"))
	}
	if l.MaxModels > 0 {
		parts = append(parts, plural(l.MaxModels, "model"))
	}
	if l.MaxStorageMB > 0 {
		parts = append(parts, fmt.Sprintf("%d MB of data", l.MaxStorageMB))
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// operator is "Acme SARL, 1 Rue de Test, Luxembourg" from whichever parts are
// configured.
func (l LegalConfig) operator() string {
	var parts []string
	for _, s := range []string{l.Entity, l.Address} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

func or(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// compactSections drops a section whose every clause depended on a value the
// deployment has not configured.
func compactSections(sections []legalSection) []legalSection {
	out := sections[:0]
	for _, s := range sections {
		if len(s.Body) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func termsDocument(l LegalConfig, product string, p *legalPlanFacts) legalDocument {
	// The edition sign-up hands out, by its own name ("the Community Edition").
	freePlan := "the free edition"
	if p != nil {
		freePlan = "the " + p.Name + " Edition"
	}
	var planClause []legalBlock
	if p != nil {
		allowance := ""
		if p.Allowance != "" {
			allowance = " to " + p.Allowance
		}
		next := ""
		if p.Next != "" {
			next = " " + p.Next
		}
		planClause = paras(
			fmt.Sprintf("The %s Edition costs nothing and has no end date. It is limited%s, and the service tells you when a limit stops an action.", p.Name, allowance),
			"A workspace that has used up its allowance becomes read-only: everything you built is still there and can still be read and exported, but nothing new can be created or changed until some of it is deleted."+next,
		)
	} else {
		planClause = paras("The edition in force for your workspace sets what it may hold. The service tells you when a limit stops an action, and a workspace over its edition's limits becomes read-only rather than being deleted: everything in it can still be read and exported.")
	}

	ending := paras(fmt.Sprintf("You may stop using the service at any time and ask us to delete your workspace by writing to %s. We delete it, and the personal data in it, within thirty days of that request, except where we must keep something longer by law.", l.Email))
	if p != nil && p.Dormancy != nil {
		d := p.Dormancy
		ending = append(ending, para(fmt.Sprintf("In the Playground, a workspace nobody uses for %d days — no one signs in to it and no API client reaches it — is deleted. We first e-mail its administrators; if nobody uses it within %d days of that message, we delete it and the personal data in it. Backups keep a copy for %d days after that: write to %s within that time to have it restored.", d.WarnDays, d.DeleteDays-d.WarnDays, d.BackupDays, l.Email)))
	}
	ending = append(ending, para("We may suspend a workspace that breaches section 4 or that has not paid an agreed fee, and will say why. Where a suspension can be resolved we will give you a reasonable chance to resolve it before deleting anything."))

	var law []legalBlock
	if l.Jurisdiction != "" {
		law = paras(fmt.Sprintf("This agreement is governed by the law of %s, and the courts of %s have jurisdiction over any dispute about it. If you are a consumer, this does not affect the rights you have where you live.", l.Jurisdiction, l.Jurisdiction))
	}

	return legalDocument{
		Title: "Terms of service",
		Intro: fmt.Sprintf("The agreement between you and %s for the use of %s.", or(l.Entity, "the operator of this service"), product),
		Sections: compactSections([]legalSection{
			{"1. Who this agreement is with", paras(
				fmt.Sprintf(`%s is operated by %s. In this document "we" and "us" mean that company, and "you" means the person or organisation using the service.`, product, or(l.operator(), "the operator of this deployment")),
				fmt.Sprintf("Questions about this agreement go to %s.", l.Email),
			)},
			{"2. Your account and your workspace", paras(
				"Signing up creates a workspace for your organisation and makes you its administrator. You can invite other people to it and decide what each of them may do.",
				"An account belongs to one named person. Sharing sign-in credentials is not allowed, because the record of who changed what depends on accounts belonging to individuals.",
				"You are responsible for what the people you invite do in your workspace, and for telling us when someone should no longer have access.",
			)},
			{"3. What the edition includes", planClause},
			{"4. What you may not do", []legalBlock{
				para("Use of the service is subject to a few limits:"),
				points(
					"do not use it to store or process unlawful content, or content you have no right to process",
					"do not try to circumvent the limits of your edition, to reach another organisation's workspace, or to disrupt the service for others",
					"do not resell or sublicense access to the service without a written agreement with us",
					"do not probe or attack the service; security research is welcome, but write to us first",
				),
				para("If something you need is blocked, ask us rather than working around it."),
			}},
			{"5. Your data stays yours", paras(
				"The models, numbers, documents and anything else you put into the service remain yours. We claim no ownership of them and use them only to run the service for you, as the privacy notice describes.",
				"You can take your data out at any time and without asking us: a model can be exported with or without its data, a grid exports to a spreadsheet, and an administrator can export the audit record.",
			)},
			{"6. Availability, backups and support", paras(
				fmt.Sprintf("We run the service with care but do not promise a particular level of availability, and %s carries no service level at all. We may take the service down for maintenance, and will give notice where we can.", freePlan),
				"We take daily backups and keep them for fourteen days. They exist so that we can recover the service, and they are not a substitute for your own exports of anything you cannot afford to lose.",
			)},
			{"7. Price", paras(
				fmt.Sprintf("%s needs no payment details. If you move to a paid edition, the price, the billing period and the payment terms are agreed with you in writing before that edition starts. Nothing here creates an obligation to pay for something you did not agree to.", strings.ToUpper(freePlan[:1])+freePlan[1:]),
			)},
			{"8. Changes", paras(
				"The service changes as it is developed; we will not remove a capability your workspace depends on without telling the workspace administrators first.",
				"If we change this agreement we will notify the workspace administrators by e-mail at least thirty days before the change takes effect, except where a change is needed sooner for legal or security reasons. Continuing to use the service after that date means you accept the new terms; if you do not, you may close your workspace instead.",
			)},
			{"9. Ending the agreement", ending},
			{"10. Liability", paras(
				fmt.Sprintf("The service is provided as it is. On %s we accept no liability beyond what the law requires of us, which in particular means we are not liable for lost profit, lost data or indirect loss.", freePlan),
				"Under a paid edition our total liability in any twelve-month period is limited to the fees you paid us in that period. Nothing in this agreement limits liability for death or personal injury caused by negligence, for fraud, or for anything else that cannot lawfully be limited.",
			)},
			{"11. Law and courts", law},
		}),
	}
}

// privacyDocument: chatConnector says whether the chat connector runs here
// (the notice describes it only where it does).
func privacyDocument(l LegalConfig, product string, chatConnector bool) legalDocument {
	var recipients []string
	if l.Hosting != "" {
		recipients = append(recipients, l.Hosting+", which hosts the servers and storage the service runs on")
	}
	recipients = append(recipients, "the e-mail relay that delivers invitations, notifications and password links")

	ai := paras("The service includes an assistant that can propose changes to a model. It works only once a provider key is configured — by your organisation, by the developer using it, or by the operator of the deployment — and when it is used, the text of the request and the structure of the model it concerns are sent to that key's provider to answer it. Nothing it proposes takes effect until a person applies it, and no decision about any individual is made automatically.")
	// The chat connector (docs/CHAT_CONNECTOR.md): reads as the person,
	// writes input cells only with the write scope, the tenant's switch
	// (connector_settings.go) and the cell history and audit log.
	if chatConnector {
		ai = append(ai, para("You can also connect ChatGPT or Claude to your account. When you ask either of them about your models, the service returns to that assistant, at your request, the names of the models, grids, metrics and dimension members you may see, the values you may read, and the text of the dashboards and workflows you may open — never anything your access does not allow, and never your password. The assistant's provider then processes that data under its own terms and privacy policy, and it stays in your conversation history there; disconnecting stops further reads but cannot remove what was already returned. When you allow it, the assistant can also enter values into the grid cells you may edit, at your request: each value is saved as entered by you, kept in the cell history and the audit log, and your workspace's administrator can turn this off."))
	}

	var where []legalBlock
	if l.Hosting != "" {
		where = paras(fmt.Sprintf("The service runs on infrastructure provided by %s, and that is where your data is stored and processed.", l.Hosting))
	}

	return legalDocument{
		Title: "Privacy notice",
		Intro: fmt.Sprintf("What %s does with personal data in %s, and the rights you have over it.", or(l.Entity, "the operator of this service"), product),
		Sections: compactSections([]legalSection{
			{"1. Who is responsible", paras(
				fmt.Sprintf("%s is the controller of the personal data described here.", or(l.operator(), "The operator of this deployment")),
				fmt.Sprintf("Write to %s with any question about this notice, or to exercise any of the rights in section 8.", l.Email),
			)},
			{"2. What we hold", []legalBlock{
				para("Three kinds of data, and it is worth keeping them apart:"),
				points(
					"Account data — your name, your work e-mail address, the workspace you belong to and what you are allowed to do in it. You give us this when you sign up, or your administrator does when they invite you.",
					"Your content — the models, numbers, comments, files and configuration your organisation puts into the service. This is yours; we hold it to run the service for you. It may contain personal data about other people, and where it does your organisation decides what goes in and we act on its instructions.",
					"Technical records — a log of requests to the service including your IP address, the time and what was called; an audit record of which account created, changed or deleted what and when; and delivery records for e-mail we send you.",
				),
				para("We do not ask for payment card details."),
			}},
			{"3. Why we hold it, and on what basis", []legalBlock{
				points(
					"To provide the service you or your organisation asked for — signing you in, showing you your workspace, sending invitations and notifications. This is necessary to perform our contract with you.",
					"To keep the service secure and accountable — the request log and the audit record let us investigate a problem or a misuse and show who changed what. This is our legitimate interest, and for many of our customers a requirement they must meet themselves.",
					"To communicate with you about the service — a change to these documents, a security notice, a workspace that has reached its limit. This is necessary for the contract, and these are not marketing messages.",
				),
				para("We do not profile you, we do not advertise to you, and we do not sell or rent personal data to anyone."),
			}},
			{"4. Cookies and what your browser stores", paras(
				"There is no analytics, tracking or advertising technology on this service, and no third-party script runs in it.",
				"What is stored is what signing in needs: the identity provider sets a cookie so that you stay signed in, the console keeps its access token in memory for the length of the session, and your browser keeps a small amount of local state such as which tab you had open. None of it is used to follow you anywhere else.",
			)},
			{"5. Who else sees it", []legalBlock{
				para("The service is run by us and by a small number of providers who process data on our instructions under a written agreement:"),
				points(recipients...),
				para("Beyond those, we disclose personal data only where the law requires it of us. We do not transfer it to anyone else, and no personal data is used to train a machine-learning model."),
			}},
			{"6. Artificial intelligence features", ai},
			{"7. How long we keep it", []legalBlock{
				points(
					"Your content and account data: for as long as your workspace exists. When a workspace is deleted, or thirty days after you ask us to close it, both are deleted.",
					"Backups: rolling, kept for fourteen days, after which they are destroyed. A deletion reaches the backups within that window.",
					"The audit record: for the period your organisation configures, or otherwise the deployment's default, since it exists for your accountability as much as ours.",
					"Request logs: kept short-term for security and operational diagnosis, then discarded.",
				),
			}},
			{"8. Your rights", paras(
				"Over your own personal data you have the right to ask us for a copy of it, to have it corrected, to have it deleted, to have our use of it restricted, to receive it in a portable form, and to object to processing we base on a legitimate interest.",
				fmt.Sprintf("Write to %s and we will answer within one month. If your data is in a customer's workspace rather than in your own account, we may need to pass your request to that organisation, because they decide what their workspace holds — we will tell you when we do.", l.Email),
				"You can also complain to a data-protection supervisory authority, in the country where you live or work or where we are established.",
			)},
			{"9. Where it is processed", where},
			{"10. Changes to this notice", paras(
				"If this notice changes in substance we will tell the workspace administrators by e-mail and move the date at the top. The version you are reading is the one in force.",
			)},
		}),
	}
}
