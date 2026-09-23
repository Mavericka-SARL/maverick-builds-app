import type { LegalInfo } from "../../api/client";

/**
 * The text of the two documents a visitor reads before they sign up.
 *
 * They live in the repository rather than in a database because they describe
 * what *this software* does with data — which data it holds, for how long, who
 * it is sent to — and that is the same wherever the engine runs. What differs
 * per deployment is who is accountable for it, so every such value comes from
 * GET /api/legal (internal/gateway/legal.go) and a clause whose value is not
 * configured is left out rather than filled with a guess.
 *
 * Two rules when editing:
 *
 *   - Only state what the code does. Every factual claim here is verifiable
 *     in the repository: the free plan's allowance and read-only ending
 *     come from the plan the server offers, the backup window from
 *     deploy/docker/pg-backup/pg-backup.sh, "no analytics" from web/index.html
 *     having no third-party script, the export rights from the four export
 *     endpoints. A sentence nobody can check is a liability, not a policy.
 *   - Move LEGAL_UPDATED when the substance changes, so a reader can tell
 *     which text they agreed to.
 */

/** A paragraph, or a list of points rendered as bullets. */
export type Block = string | string[];

export interface LegalSection {
  heading: string;
  body: Block[];
}

export interface LegalDocument {
  title: string;
  /** The line under the title: what this document is. */
  intro: string;
  sections: LegalSection[];
}

/**
 * What the server's self-service plan offers, when there is one: bounded by
 * its allowance, never by time.
 */
export interface PlanFacts {
  name: string;
  allowance: string;
  /** The plan's own "where to go from here", if it has one. */
  next?: string;
}

/** "Acme SARL, 1 Rue de Test, Luxembourg" from whichever parts are configured. */
function operator(l: LegalInfo): string {
  return [l.entity, l.address].filter(Boolean).join(", ");
}

function compact(sections: LegalSection[]): LegalSection[] {
  return sections.filter((s) => s.body.length > 0);
}

export function termsDocument(l: LegalInfo, product: string, plan?: PlanFacts): LegalDocument {
  const planClause: Block[] = plan
    ? [
        `The ${plan.name} plan costs nothing and has no end date. It is limited${plan.allowance ? ` to ${plan.allowance}` : ""}, and the service tells you when a limit stops an action.`,
        `A workspace that has used up its allowance becomes read-only: everything you built is still there and can still be read and exported, but nothing new can be created or changed until some of it is deleted.${plan.next ? ` ${plan.next}` : ""}`,
      ]
    : [
        "The plan in force for your workspace sets what it may hold. The service tells you when a limit stops an action, and a workspace over its plan's limits becomes read-only rather than being deleted: everything in it can still be read and exported.",
      ];

  return {
    title: "Terms of service",
    intro: `The agreement between you and ${l.entity || "the operator of this service"} for the use of ${product}.`,
    sections: compact([
      {
        heading: "1. Who this agreement is with",
        body: [
          `${product} is operated by ${operator(l) || "the operator of this deployment"}. In this document "we" and "us" mean that company, and "you" means the person or organisation using the service.`,
          `Questions about this agreement go to ${l.email}.`,
        ],
      },
      {
        heading: "2. Your account and your workspace",
        body: [
          "Signing up creates a workspace for your organisation and makes you its administrator. You can invite other people to it and decide what each of them may do.",
          "An account belongs to one named person. Sharing sign-in credentials is not allowed, because the record of who changed what depends on accounts belonging to individuals.",
          "You are responsible for what the people you invite do in your workspace, and for telling us when someone should no longer have access.",
        ],
      },
      { heading: "3. What the plan includes", body: planClause },
      {
        heading: "4. What you may not do",
        body: [
          "Use of the service is subject to a few limits:",
          [
            "do not use it to store or process unlawful content, or content you have no right to process",
            "do not try to circumvent the limits of your plan, to reach another organisation's workspace, or to disrupt the service for others",
            "do not resell or sublicense access to the service without a written agreement with us",
            "do not probe or attack the service; security research is welcome, but write to us first",
          ],
          "If something you need is blocked, ask us rather than working around it.",
        ],
      },
      {
        heading: "5. Your data stays yours",
        body: [
          "The models, numbers, documents and anything else you put into the service remain yours. We claim no ownership of them and use them only to run the service for you, as the privacy notice describes.",
          "You can take your data out at any time and without asking us: a model can be exported with or without its data, a grid exports to a spreadsheet, and an administrator can export the audit record.",
        ],
      },
      {
        heading: "6. Availability, backups and support",
        body: [
          "We run the service with care but do not promise a particular level of availability, and the free plan carries no service level at all. We may take the service down for maintenance, and will give notice where we can.",
          "We take daily backups and keep them for fourteen days. They exist so that we can recover the service, and they are not a substitute for your own exports of anything you cannot afford to lose.",
        ],
      },
      {
        heading: "7. Price",
        body: [
          "The free plan needs no payment details. If you move to a paid plan, the price, the billing period and the payment terms are agreed with you in writing before that plan starts. Nothing here creates an obligation to pay for something you did not agree to.",
        ],
      },
      {
        heading: "8. Changes",
        body: [
          "The service changes as it is developed; we will not remove a capability your workspace depends on without telling the workspace administrators first.",
          "If we change this agreement we will notify the workspace administrators by e-mail at least thirty days before the change takes effect, except where a change is needed sooner for legal or security reasons. Continuing to use the service after that date means you accept the new terms; if you do not, you may close your workspace instead.",
        ],
      },
      {
        heading: "9. Ending the agreement",
        body: [
          `You may stop using the service at any time and ask us to delete your workspace by writing to ${l.email}. We delete it, and the personal data in it, within thirty days of that request, except where we must keep something longer by law.`,
          "We may suspend a workspace that breaches section 4 or that has not paid an agreed fee, and will say why. Where a suspension can be resolved we will give you a reasonable chance to resolve it before deleting anything.",
        ],
      },
      {
        heading: "10. Liability",
        body: [
          "The service is provided as it is. On the free plan we accept no liability beyond what the law requires of us, which in particular means we are not liable for lost profit, lost data or indirect loss.",
          "Under a paid plan our total liability in any twelve-month period is limited to the fees you paid us in that period. Nothing in this agreement limits liability for death or personal injury caused by negligence, for fraud, or for anything else that cannot lawfully be limited.",
        ],
      },
      {
        heading: "11. Law and courts",
        body: l.jurisdiction
          ? [`This agreement is governed by the law of ${l.jurisdiction}, and the courts of ${l.jurisdiction} have jurisdiction over any dispute about it. If you are a consumer, this does not affect the rights you have where you live.`]
          : [],
      },
    ]),
  };
}

export function privacyDocument(l: LegalInfo, product: string): LegalDocument {
  const recipients: string[] = [
    l.hosting ? `${l.hosting}, which hosts the servers and storage the service runs on` : "",
    "the e-mail relay that delivers invitations, notifications and password links",
  ].filter(Boolean);

  return {
    title: "Privacy notice",
    intro: `What ${l.entity || "the operator of this service"} does with personal data in ${product}, and the rights you have over it.`,
    sections: compact([
      {
        heading: "1. Who is responsible",
        body: [
          `${operator(l) || "The operator of this deployment"} is the controller of the personal data described here.`,
          `Write to ${l.email} with any question about this notice, or to exercise any of the rights in section 8.`,
        ],
      },
      {
        heading: "2. What we hold",
        body: [
          "Three kinds of data, and it is worth keeping them apart:",
          [
            "Account data — your name, your work e-mail address, the workspace you belong to and what you are allowed to do in it. You give us this when you sign up, or your administrator does when they invite you.",
            "Your content — the models, numbers, comments, files and configuration your organisation puts into the service. This is yours; we hold it to run the service for you. It may contain personal data about other people, and where it does your organisation decides what goes in and we act on its instructions.",
            "Technical records — a log of requests to the service including your IP address, the time and what was called; an audit record of which account created, changed or deleted what and when; and delivery records for e-mail we send you.",
          ],
          "We do not ask for payment card details; the free plan needs none.",
        ],
      },
      {
        heading: "3. Why we hold it, and on what basis",
        body: [
          [
            "To provide the service you or your organisation asked for — signing you in, showing you your workspace, sending invitations and notifications. This is necessary to perform our contract with you.",
            "To keep the service secure and accountable — the request log and the audit record let us investigate a problem or a misuse and show who changed what. This is our legitimate interest, and for many of our customers a requirement they must meet themselves.",
            "To communicate with you about the service — a change to these documents, a security notice, a workspace that has reached its limit. This is necessary for the contract, and these are not marketing messages.",
          ],
          "We do not profile you, we do not advertise to you, and we do not sell or rent personal data to anyone.",
        ],
      },
      {
        heading: "4. Cookies and what your browser stores",
        body: [
          "There is no analytics, tracking or advertising technology on this service, and no third-party script runs in it.",
          "What is stored is what signing in needs: the identity provider sets a cookie so that you stay signed in, the console keeps its access token in memory for the length of the session, and your browser keeps a small amount of local state such as which tab you had open. None of it is used to follow you anywhere else.",
        ],
      },
      {
        heading: "5. Who else sees it",
        body: [
          "The service is run by us and by a small number of providers who process data on our instructions under a written agreement:",
          recipients,
          "Beyond those, we disclose personal data only where the law requires it of us. We do not transfer it to anyone else, and no personal data is used to train a machine-learning model.",
        ],
      },
      {
        heading: "6. Artificial intelligence features",
        body: [
          "The service includes an assistant that can propose changes to a model. It is off until your organisation switches it on with its own provider key, and while it is on, the text of a request and the structure of the model it concerns are sent to that provider to answer it. Nothing it proposes takes effect until a person applies it, and no decision about any individual is made automatically.",
        ],
      },
      {
        heading: "7. How long we keep it",
        body: [
          [
            "Your content and account data: for as long as your workspace exists. When a workspace is deleted, or thirty days after you ask us to close it, both are deleted.",
            "Backups: rolling, kept for fourteen days, after which they are destroyed. A deletion reaches the backups within that window.",
            "The audit record: for the period your organisation configures for its workspace, since it exists for your accountability as much as ours.",
            "Request logs: kept short-term for security and operational diagnosis, then discarded.",
          ],
        ],
      },
      {
        heading: "8. Your rights",
        body: [
          "Over your own personal data you have the right to ask us for a copy of it, to have it corrected, to have it deleted, to have our use of it restricted, to receive it in a portable form, and to object to processing we base on a legitimate interest.",
          `Write to ${l.email} and we will answer within one month. If your data is in a customer's workspace rather than in your own account, we may need to pass your request to that organisation, because they decide what their workspace holds — we will tell you when we do.`,
          "You can also complain to a data-protection supervisory authority, in the country where you live or work or where we are established.",
        ],
      },
      {
        heading: "9. Where it is processed",
        body: l.hosting ? [`The service runs on infrastructure provided by ${l.hosting}, and that is where your data is stored and processed.`] : [],
      },
      {
        heading: "10. Changes to this notice",
        body: [
          "If this notice changes in substance we will tell the workspace administrators by e-mail and move the date at the top. The version you are reading is the one in force.",
        ],
      },
    ]),
  };
}
